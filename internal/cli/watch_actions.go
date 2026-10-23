package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aymanbagabas/go-osc52/v2"
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
	stop, remove, force, cancel, closeHelp key.Binding
}{
	closeHelp: key.NewBinding(key.WithKeys("esc", "?", "q")),
	stop:      key.NewBinding(key.WithKeys("s", "enter")),
	remove:    key.NewBinding(key.WithKeys("d", "enter")),
	force:     key.NewBinding(key.WithKeys("F")),
	cancel:    key.NewBinding(key.WithKeys("esc", "n")),
}

// dialogKind is the dialog open over the screen.
type dialogKind int

const (
	dialogNone   dialogKind = iota
	dialogStop              // stop the run
	dialogRemove            // remove the run
	dialogHelp              // the keys
)

// watchDialog is the open dialog and the run it acts on, as it was when it opened.
type watchDialog struct {
	kind   dialogKind
	run    RunView
	dirty  string // why rm refused the worktree: F removes it anyway
	scroll int    // lines of a dialog taller than the screen scrolled past
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

// copyID copies the selected run's ID, and only it, to the clipboard of the terminal, off Update.
func (m watchModel) copyID() (watchModel, tea.Cmd) {
	v, ok := m.selected()
	if !ok {
		return m, nil
	}
	acts, id := m.opts.actions, v.ID
	return m, func() tea.Msg {
		if err := acts.Copy(strconv.FormatInt(id, 10)); err != nil {
			return watchQuickMsg{err: err}
		}
		return watchQuickMsg{out: fmt.Sprintf("run %d copied", id)}
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
	switch {
	case key.Matches(msg, watchKeys.up):
		m.dialog.scroll = max(min(m.dialog.scroll, m.maxScroll())-1, 0)
		return m, nil
	case key.Matches(msg, watchKeys.down):
		m.dialog.scroll = min(m.dialog.scroll+1, m.maxScroll())
		return m, nil
	case m.dialog.kind == dialogHelp:
		if key.Matches(msg, dialogKeys.closeHelp) {
			m.dialog = watchDialog{}
		}
		return m, nil
	}
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
// When it is taller than the screen, the lines it marks scroll.
func (m watchModel) dialogBox() []string {
	title, look, d := m.dialogLines()
	lines := d.lines
	if over, to := len(lines)+2-m.height, d.scrollEnd(); over > 0 && to > d.from {
		from, n := d.from+min(m.dialog.scroll, m.maxScroll()), max(to-d.from-over, 0)
		lines = slices.Concat(lines[:d.from], lines[from:from+n], lines[to:])
		title += " · ↑↓"
	}
	return panel(title, lines, d.inner+4, len(lines)+2, look)
}

// maxScroll is how far the open dialog scrolls: as many lines as it is taller than the screen.
func (m watchModel) maxScroll() int {
	_, _, d := m.dialogLines()
	return max(min(len(d.lines)+2-m.height, d.scrollEnd()-d.from), 0)
}

// dialogLines are the title, the border and the lines of the open dialog.
func (m watchModel) dialogLines() (string, lipgloss.Style, dialogWriter) {
	d := dialogWriter{st: m.st, inner: min(64, m.width-4) - 4, labelW: 11}
	if m.layout == layoutCompact || m.layout == layoutMini {
		d.labelW = 9
	}
	st, v := m.st, m.dialog.run
	title, look := "delamain stop", st.warn
	switch {
	case m.dialog.kind == dialogHelp:
		title, look = "help", st.info
		m.helpLines(&d)
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
	return title, look, d
}

// helpKeys are the rows of the help: the keys and what they do.
var helpKeys = [][2]string{
	{"↑ ↓ · j k", "pick the run; the status and the logs follow it"},
	{"tab", "switch the focus between the runs and the logs"},
	{"pgup pgdn · g G", "scroll the logs; G follows the end again"},
	{"1 … 6", "filter the source"},
	{"f", "follow the end, or stop following it"},
	{"z", "the logs in full screen"},
	{"enter", "the full status (medium and compact)"},
	{"e", "expand the finished runs (compact)"},
	{"← →", "switch the tab (mini)"},
	{"s · d", "stop the run (delamain stop) · remove it (delamain rm)"},
	{"o · y", "open the PR in the browser · copy the run ID"},
	{"q · ctrl+c", "quit; the runs go on in the daemon"},
}

// helpLines are the help: how often `watch` reloads, then the keys, which scroll.
func (m watchModel) helpLines(d *dialogWriter) {
	d.text(m.st.none, "the list and the logs reload on their own every "+m.opts.interval.String())
	d.blank()
	d.labelW = 17 // the widest key and two spaces
	d.from = len(d.lines)
	for _, k := range helpKeys {
		d.row(paint(m.st.info.Bold(true), k[0]), k[1])
	}
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
	st       styles
	inner    int
	labelW   int // the column of the labels of row
	lines    []string
	from, to int // the lines that scroll, to 0 for up to the end
}

// scrollEnd is the end of the lines that scroll.
func (d dialogWriter) scrollEnd() int {
	if d.to == 0 {
		return len(d.lines)
	}
	return d.to
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
			head = paint(d.st.label, label) + strings.Repeat(" ", max(d.labelW-lipgloss.Width(label), 0))
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
	opener string    // the command that opens a URL in the browser
	out    io.Writer // the terminal, for OSC 52
	tmux   bool      // OSC 52 goes through tmux
}

func newStoreWatchActions(cfg config.Config, st *store.Store, out io.Writer) *storeWatchActions {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return &storeWatchActions{stop: &stopper{cfg: cfg, st: st, noWait: true}, opener: opener, out: out, tmux: os.Getenv("TMUX") != ""}
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

// Copy writes text to the terminal as an OSC 52 sequence, in one write beside the renderer's.
func (a *storeWatchActions) Copy(text string) error {
	seq := osc52.New(text)
	if a.tmux {
		seq = seq.Tmux()
	}
	_, err := seq.WriteTo(a.out)
	return err
}

// close releases what the stops opened: the notifier of the recover.
func (a *storeWatchActions) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stop.close()
}
