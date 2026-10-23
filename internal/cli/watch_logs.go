package cli

import (
	"fmt"
	"math"
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
	paused bool         // the user scrolled away: new lines do not move the view
	vp     viewport.Model
}

// reset points the pane at the logs of run, in dir, and forgets what was read; it does no I/O.
func (p *logPane) reset(run int64, dir string) {
	p.fl, p.run, p.lines, p.paused = nil, run, nil, false
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

// pageUp goes up half a page and pauses; pageDown goes down half a page and stays paused.
func (p *logPane) pageUp() {
	p.vp.HalfPageUp()
	p.paused = true
}

func (p *logPane) pageDown() { p.vp.HalfPageDown() }

// lineUp and top pause; lineDown stays paused, even at the end.
func (p *logPane) lineUp() {
	p.vp.ScrollUp(1)
	p.paused = true
}

func (p *logPane) lineDown() { p.vp.ScrollDown(1) }

func (p *logPane) top() {
	p.vp.GotoTop()
	p.paused = true
}

// follow goes back to the end and follows the new lines.
func (p *logPane) follow() {
	p.paused = false
	p.vp.GotoBottom()
}

// toggleFollow pauses where the view is, or follows from the end.
func (p *logPane) toggleFollow() {
	if p.paused {
		p.follow()
		return
	}
	p.paused = true
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
	if !p.paused {
		p.vp.GotoBottom()
	}
	p.vp.SetYOffset(p.vp.YOffset) // within the content, which a filter may have shortened
}

// state is the title of the logs of run: ended, paused or following.
func (p logPane) state(st styles, run RunView) string {
	switch {
	case run.terminal:
		return paint(st.muted, "ended")
	case p.paused:
		return paint(st.warn, "paused")
	}
	return paint(st.ok, "● following")
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
	left := paint(st.muted, "following the end · G")
	switch {
	case run.terminal:
		left = paint(st.muted, "ended · ") + resultCell(st, run.Result).styled
	case p.paused:
		left = paint(st.muted, "paused · G back to the end")
	}
	right := paint(st.muted, fmt.Sprintf("%d of %d lines · %d%%", len(p.shown), len(p.lines), int(math.Round(p.vp.ScrollPercent()*100))))
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	return ansi.Truncate(left+strings.Repeat(" ", max(gap, 1))+right, inner, "…")
}

// logRow is a line as emit prints it: the source padded by its plain name, a muted │ and the
// colored text.
func logRow(st styles, l logLine) string {
	pad := strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source)))
	return paint(st.sourceStyle(l.source), l.source) + pad + " " + paint(st.muted, "│") + " " + colorLine(st, l.source, l.text)
}
