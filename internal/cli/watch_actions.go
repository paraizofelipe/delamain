package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	stop, cancel key.Binding
}{
	stop:   key.NewBinding(key.WithKeys("s", "enter")),
	cancel: key.NewBinding(key.WithKeys("esc", "n")),
}

// dialogKind is the dialog open over the screen.
type dialogKind int

const (
	dialogNone dialogKind = iota
	dialogStop            // stop an active run
)

// watchDialog is the open dialog and the run it acts on, as it was when it opened.
type watchDialog struct {
	kind dialogKind
	run  RunView
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

// watchActionMsg is what an action printed, or why it failed.
type watchActionMsg struct {
	out string
	err error
}

// openStop opens the stop dialog of the selected run.
func (m watchModel) openStop() watchModel {
	if v, ok := m.selected(); ok {
		m.dialog = watchDialog{kind: dialogStop, run: v}
	}
	return m
}

// dialogKey handles a key while a dialog is open.
func (m watchModel) dialogKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	switch {
	case key.Matches(msg, dialogKeys.cancel):
		m.dialog = watchDialog{}
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

// actionDone shows how an action ended: what it printed in ok, or why it failed in bad.
func (m watchModel) actionDone(msg watchActionMsg) watchModel {
	m.acting = false
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
