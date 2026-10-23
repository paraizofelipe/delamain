package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

// watchActions are what `watch` does to a run; the model calls them in a tea.Cmd, off Update.
type watchActions interface {
	// Stop is `delamain stop --no-wait id`: what it prints, or why it failed.
	Stop(ctx context.Context, id int64) (string, error)
	// Remove is `delamain rm id`, with --force when force is set: what it prints, or why it failed.
	Remove(ctx context.Context, id int64, force bool) (string, error)
	// OpenURL opens url in the browser without waiting for it.
	OpenURL(url string) error
	// Copy puts text in the clipboard of the terminal.
	Copy(text string) error
}

// dialogKeys are the keys of the open dialog: nothing else acts while it is open.
var dialogKeys = struct {
	stop, remove, force, cancel key.Binding
}{
	stop:   key.NewBinding(key.WithKeys("s", "enter")),
	remove: key.NewBinding(key.WithKeys("d", "enter")),
	force:  key.NewBinding(key.WithKeys("F")),
	cancel: key.NewBinding(key.WithKeys("esc", "n")),
}

// dialogKind is the dialog open over the screen.
type dialogKind int

const (
	dialogNone   dialogKind = iota
	dialogStop              // stop the run
	dialogRemove            // remove the run
)

// watchDialog is the open dialog and the run it acts on, as it was when it opened.
type watchDialog struct {
	kind  dialogKind
	run   RunView
	dirty string // why rm refused the worktree: F removes it anyway
}

// toastFor is how long a toast stays on the footer.
const toastFor = 5 * time.Second

// watchToast is the temporary message on the first line of the footer; one without a time stays
// until another replaces it.
type watchToast struct {
	text string
	look lipgloss.Style
	at   time.Time
}

// newToast is a toast shown from now on for toastFor.
func (m watchModel) newToast(look lipgloss.Style, text string) watchToast {
	return watchToast{text: text, look: look, at: m.opts.now()}
}

// expireToast drops the toast once its time is up; the reload ticks call it.
func (m watchModel) expireToast() watchModel {
	if !m.toast.at.IsZero() && m.opts.now().Sub(m.toast.at) >= toastFor {
		m.toast = watchToast{}
	}
	return m
}

// watchActionMsg is what an action printed, or why it failed; removed is the run a remove was
// asked for.
type watchActionMsg struct {
	out     string
	err     error
	removed int64
}

// watchQuickMsg is how an open or a copy ended: they run beside a stop or a remove.
type watchQuickMsg watchActionMsg

// openPR opens the selected run's PR/MR in the browser, off Update.
func (m watchModel) openPR() (watchModel, tea.Cmd) {
	v, ok := m.selected()
	switch {
	case !ok:
		return m, nil
	case v.PR == "":
		m.toast = m.newToast(m.st.muted, fmt.Sprintf("run %d has no PR/MR", v.ID))
		return m, nil
	}
	acts, url := m.opts.actions, v.PR
	return m, func() tea.Msg {
		if err := acts.OpenURL(url); err != nil {
			return watchQuickMsg{err: err}
		}
		return nil
	}
}

// openDialog opens a dialog of kind on the selected run.
func (m watchModel) openDialog(kind dialogKind) watchModel {
	if v, ok := m.selected(); ok {
		m.dialog = watchDialog{kind: kind, run: v}
	}
	return m
}

// dialogKey handles a key while a dialog is open.
func (m watchModel) dialogKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	if key.Matches(msg, dialogKeys.cancel) {
		m.dialog = watchDialog{}
		return m, nil
	}
	if m.dialog.kind == dialogRemove {
		return m.removeKey(msg)
	}
	switch {
	case key.Matches(msg, dialogKeys.stop):
		if m.acting || m.dialog.run.terminal { // a finished run has nothing to stop // the action that runs reads and writes the same store: one at a time
			return m, nil
		}
		id, acts := m.dialog.run.ID, m.opts.actions
		m.dialog, m.acting = watchDialog{}, true
		m.toast = watchToast{text: fmt.Sprintf("stopping run %d…", id), look: m.st.warn}
		return m, func() tea.Msg {
			out, err := acts.Stop(context.Background(), id)
			return watchActionMsg{out: out, err: err}
		}
	}
	return m, nil
}

// removeKey handles a key of the rm dialog: rm only deletes finished runs, and F forces it after
// a dirty worktree. The dialog stays open while rm runs.
func (m watchModel) removeKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	force := key.Matches(msg, dialogKeys.force)
	switch {
	case m.acting || !m.dialog.run.terminal:
		return m, nil
	case force && m.dialog.dirty == "", !force && !key.Matches(msg, dialogKeys.remove):
		return m, nil
	}
	id, acts := m.dialog.run.ID, m.opts.actions
	m.acting = true
	return m, func() tea.Msg {
		out, err := acts.Remove(context.Background(), id, force)
		return watchActionMsg{out: out, err: err, removed: id}
	}
}

// actionDone shows how an action ended: what it printed in ok, or why it failed in bad. A
// worktree rm refused for its changes keeps the dialog open, offering F.
func (m watchModel) actionDone(msg watchActionMsg) watchModel {
	m.acting = false
	open := m.dialog.kind == dialogRemove && m.dialog.run.ID == msg.removed && msg.removed != 0
	if open && errors.Is(msg.err, errDirtyWorktree) {
		m.dialog.dirty = clean(msg.err.Error())
		return m
	}
	if open {
		m.dialog = watchDialog{}
	}
	return m.result(msg)
}

// result is the toast of how an action ended: what it printed in ok, or why it failed in bad.
func (m watchModel) result(msg watchActionMsg) watchModel {
	if msg.err != nil {
		m.toast = m.newToast(m.st.bad, clean(msg.err.Error()))
		return m
	}
	var lines []string // the columns of the lines of recover are lost on one line: so are their spaces
	for _, l := range strings.Split(msg.out, "\n") {
		if l = strings.Join(strings.Fields(clean(l)), " "); l != "" {
			lines = append(lines, l)
		}
	}
	m.toast = m.newToast(m.st.ok, strings.Join(lines, " · "))
	return m
}

// toastLines are the toast wrapped to the width of the screen, none without one.
func (m watchModel) toastLines() []string {
	if m.toast.text == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(ansi.Wrap(m.toast.text, max(m.width, 1), ""), "\n") {
		out = append(out, paint(m.toast.look, l))
	}
	return out
}

// dialogBox draws the open dialog: min(64, width-4) columns wide, its text wrapped, never cut.
func (m watchModel) dialogBox() []string {
	w := min(64, m.width-4)
	d := dialogWriter{st: m.st, inner: w - 4, labelW: 11}
	if m.layout == layoutCompact || m.layout == layoutMini {
		d.labelW = 9
	}
	st, v := m.st, m.dialog.run
	title, look := "delamain stop", st.warn
	switch {
	case m.dialog.kind == dialogRemove:
		title, look = "delamain rm", st.bad
		m.removeLines(&d)
	case v.terminal:
		d.text(st.none.Bold(true), fmt.Sprintf("run %d already finished", v.ID))
		d.blank()
		d.row("state", paint(st.runStateStyle(v.State), v.State))
		d.row("result", orDash(resultCell(st, v.Result).styled))
		d.blank()
		d.buttons([2]string{"ok", "esc"})
	default:
		owner, ownerLook := fmt.Sprintf("pid %d (alive): only this run is cancelled", v.Owner.PID), st.ok
		if !v.Owner.Alive {
			owner, ownerLook = fmt.Sprintf("pid %d (dead): the run is finalized here, as delamain recover does", v.Owner.PID), st.bad
		}
		d.text(st.none.Bold(true), fmt.Sprintf("Stop run %d?", v.ID))
		d.styled(paint(st.info, v.Issue) + " " + v.Title)
		d.blank()
		d.row("owner", paint(ownerLook, owner))
		d.row("result", "halted: stopped by the user (delamain stop)")
		d.row("delivery", "draft of what was already committed + needs-human label")
		d.blank()
		d.buttons([2]string{"stop", "s"}, [2]string{"cancel", "esc"})
	}
	return panel(title, d.lines, w, len(d.lines)+2, look)
}

// removeLines are the rm dialog: rm refuses a run that has not finished.
func (m watchModel) removeLines(d *dialogWriter) {
	st, v := m.st, m.dialog.run
	if !v.terminal {
		d.text(st.bad.Bold(true), fmt.Sprintf("run %d is still %s", v.ID, v.State))
		d.text(st.bad, "stop it first with s: rm only deletes finished runs")
		d.blank()
		d.buttons([2]string{"ok", "esc"})
		return
	}
	keeps := "the PR"
	if v.Branch != "" {
		keeps = "branch " + v.Branch + " and the PR"
	}
	d.text(st.none.Bold(true), fmt.Sprintf("Remove run %d?", v.ID))
	d.styled(paint(st.info, v.Issue) + " " + v.Title)
	d.blank()
	d.row("deletes", "record, run directory and worktree")
	d.row("keeps", keeps)
	d.row("budget", "the day's spend does not change (token_ledger stays)")
	d.blank()
	if m.dialog.dirty != "" {
		d.text(st.bad, m.dialog.dirty)
		d.blank()
		d.buttons([2]string{"remove anyway", "F"}, [2]string{"cancel", "esc"})
		return
	}
	d.buttons([2]string{"remove", "d"}, [2]string{"cancel", "esc"})
}

// dialogWriter writes the lines of a dialog inner columns wide, wrapping every text.
type dialogWriter struct {
	st     styles
	inner  int
	labelW int // the column of the labels of row
	lines  []string
}

func (d *dialogWriter) blank() { d.lines = append(d.lines, "") }

// text writes s in look.
func (d *dialogWriter) text(look lipgloss.Style, s string) { d.styled(paint(look, s)) }

// styled writes s, already painted.
func (d *dialogWriter) styled(s string) { d.lines = append(d.lines, wrapStyled(s, d.inner)...) }

// row writes label in its column and value, already painted, at its right, wrapped under itself.
func (d *dialogWriter) row(label, value string) {
	for i, l := range wrapStyled(value, max(d.inner-d.labelW, 1)) {
		head := strings.Repeat(" ", d.labelW)
		if i == 0 {
			head = paint(d.st.label, fmt.Sprintf("%-*s", d.labelW, label))
		}
		d.lines = append(d.lines, head+l)
	}
}

// buttons writes the buttons, a button never split across lines.
func (d *dialogWriter) buttons(bs ...[2]string) {
	line := ""
	for _, b := range bs {
		item := "[ " + b[0] + "  " + paint(d.st.info.Bold(true), b[1]) + " ]"
		switch {
		case line == "":
			line = item
		case lipgloss.Width(line)+2+lipgloss.Width(item) <= d.inner:
			line += "  " + item
		default:
			d.lines, line = append(d.lines, line), item
		}
	}
	d.lines = append(d.lines, line)
}

// overlay puts box in the middle of screen, whose other cells are dimmed.
func (m watchModel) overlay(screen string, box []string) string {
	lines := strings.Split(screen, "\n")
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	bw := lipgloss.Width(box[0])
	x, y := max((m.width-bw)/2, 0), max((m.height-len(box))/2, 0)
	for i, l := range lines {
		plain := ansi.Strip(l)
		if i < y || i >= y+len(box) {
			lines[i] = paint(m.st.muted, plain)
			continue
		}
		left := ansi.Truncate(plain, x, "")
		left += strings.Repeat(" ", x-lipgloss.Width(left))
		lines[i] = paint(m.st.muted, left) + box[i-y] + paint(m.st.muted, ansi.TruncateLeft(plain, x+bw, ""))
	}
	return strings.Join(lines[:min(len(lines), m.height)], "\n")
}

// storeWatchActions are the actions of `watch` on the state database, as the commands do them.
type storeWatchActions struct {
	mu     sync.Mutex // the stopper builds its runner on first use: one action at a time
	stop   *stopper
	opener string // the command that opens a URL in the browser
}

func newStoreWatchActions(cfg config.Config, st *store.Store, _ io.Writer) *storeWatchActions {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return &storeWatchActions{stop: &stopper{cfg: cfg, st: st, noWait: true}, opener: opener}
}

// Stop is `delamain stop --no-wait id`.
func (a *storeWatchActions) Stop(ctx context.Context, id int64) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out strings.Builder
	err := a.stop.stopOne(ctx, &out, id)
	return out.String(), err
}

// Remove is `delamain rm id`, or `delamain rm --force id`.
func (a *storeWatchActions) Remove(ctx context.Context, id int64, force bool) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out strings.Builder
	err := removeRun(ctx, &out, a.stop.cfg, a.stop.st, id, force)
	return out.String(), err
}

// OpenURL starts the opener on an https URL of a host, without waiting for it: the URL comes
// from the forge or the state database, and anything else could run a local file or a handler.
func (a *storeWatchActions) OpenURL(target string) error {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("not an https URL, not opened: %s", target)
	}
	cmd := exec.Command(a.opener, target) //nolint:gosec // an https URL as the only argument, no shell
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (a *storeWatchActions) Copy(string) error { return errors.New("copy is not available") }

// close releases what the stops opened: the notifier of the recover.
func (a *storeWatchActions) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stop.close()
}
