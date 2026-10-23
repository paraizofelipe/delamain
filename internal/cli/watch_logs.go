package cli

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// logSources are the chips of the logs panel, picked with the keys 1 to 6; the first shows them all.
var logSources = []string{"all", "delamain", "checks", "planner", "implementer", "reviewer"}

// logPane is the logs panel of `watch`: the lines read so far of the selected run and the
// viewport over them. Update only swaps fl; its fields are touched by the reload command alone.
type logPane struct {
	fl     *logFollower // nil without a selected run
	run    int64        // the run fl reads
	lines  []logLine    // control characters already dropped
	source int          // index in logSources; it outlives reset
	shown  []logLine    // the lines of source
	vp     viewport.Model
}

// reset points the pane at the logs of run, in dir, and forgets what was read; it does no I/O.
func (p *logPane) reset(run int64, dir string) {
	p.fl, p.run, p.lines = nil, run, nil
	if dir != "" {
		p.fl = &logFollower{dir: dir, offsets: map[string]int64{}, partial: map[string][]byte{}, lastTS: map[string]string{}}
	}
	p.vp = viewport.New(0, p.vp.Height)
	p.refresh()
}

// append adds lines read from the files: their text is from third parties, so it loses its
// control characters here.
func (p *logPane) append(lines []logLine) {
	for _, l := range lines {
		p.lines = append(p.lines, logLine{ts: l.ts, source: clean(l.source), text: clean(l.text)})
	}
	p.refresh()
}

// setFilter shows only the lines of logSources[i]; the others stay in memory.
func (p *logPane) setFilter(i int) {
	p.source = i
	p.refresh()
}

// resize fits the pane in height lines: the log lines take what the chips, the separators and
// the footer leave.
func (p *logPane) resize(height int) {
	_, p.vp.Height = paneRows(height)
	p.refresh()
}

// paneRows splits height between the separators and the log lines: the log lines give way
// first, then the separators, so the chips and the footer stay.
func paneRows(height int) (seps, rows int) {
	return min(max(height-2, 0), 2), max(height-4, 0)
}

// refresh gives the viewport the lines of the source and keeps it at the end.
func (p *logPane) refresh() {
	p.shown = nil // a new array: copies of the model share the old one
	texts := []string{}
	for _, l := range p.lines {
		if p.source == 0 || l.source == logSources[p.source] {
			p.shown = append(p.shown, l)
			texts = append(texts, l.text)
		}
	}
	p.vp.SetContent(strings.Join(texts, "\n"))
	p.vp.GotoBottom()
}

// view is the inside of the panel for run, inner columns wide and at most height lines high:
// the log lines, then the footer.
func (p logPane) view(st styles, run RunView, inner, height int) []string {
	if p.fl == nil || height <= 0 {
		return nil
	}
	seps, rows := paneRows(height)
	rows = min(rows, p.vp.Height)
	sep := paint(st.muted, strings.Repeat("─", max(inner, 0)))
	out := []string{ansi.Truncate(p.chips(st), inner, "…")}
	if seps > 0 {
		out = append(out, sep)
	}
	body := []string{}
	if len(p.lines) == 0 {
		body = append(body, paint(st.muted, ansi.Truncate("no logs yet in "+clean(p.fl.dir), inner, "…")))
	}
	top := min(p.vp.YOffset, len(p.shown))
	for _, l := range p.shown[top:min(top+rows, len(p.shown))] {
		body = append(body, ansi.Truncate(logRow(st, l), inner, "…"))
	}
	for i := range rows {
		if i < len(body) {
			out = append(out, body[i])
		} else {
			out = append(out, "")
		}
	}
	if seps > 1 {
		out = append(out, sep)
	}
	return append(out, p.footer(st, run, inner))[:min(len(out)+1, height)]
}

// chips name the sources to filter by; the active one is in reverse video, in its color.
func (p logPane) chips(st styles) string {
	out := paint(st.muted, "source") + " "
	for i, s := range logSources {
		look := st.sourceStyle(s)
		if i == 0 {
			look = st.none
		}
		if i == p.source {
			out += paint(look.Reverse(true), "["+s+"]")
		} else {
			out += paint(look, " "+s+" ")
		}
	}
	return out
}

// footer tells where the logs stand at the left and how many lines are shown at the right.
func (p logPane) footer(st styles, run RunView, inner int) string {
	left := ""
	if run.terminal {
		left = paint(st.muted, "ended · ") + resultCell(st, run.Result).styled
	}
	right := paint(st.muted, fmt.Sprintf("%d of %d lines", len(p.shown), len(p.lines)))
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	return ansi.Truncate(left+strings.Repeat(" ", max(gap, 1))+right, inner, "…")
}

// logRow is a line as emit prints it: the source padded by its plain name, a muted │ and the
// colored text.
func logRow(st styles, l logLine) string {
	pad := strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source)))
	return paint(st.sourceStyle(l.source), l.source) + pad + " " + paint(st.muted, "│") + " " + colorLine(st, l.source, l.text)
}
