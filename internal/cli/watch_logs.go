package cli

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/x/ansi"
)

// logPane is the logs panel of `watch`: the lines read so far of the selected run and the
// viewport over them. Update only swaps fl; its fields are touched by the reload command alone.
type logPane struct {
	fl    *logFollower // nil without a selected run
	run   int64        // the run fl reads
	lines []logLine    // control characters already dropped
	vp    viewport.Model
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

// resize sets how many log lines the panel shows.
func (p *logPane) resize(height int) {
	p.vp.Height = max(height, 0)
	p.refresh()
}

// refresh gives the viewport what it scrolls over and keeps it at the end.
func (p *logPane) refresh() {
	texts := make([]string, len(p.lines))
	for i, l := range p.lines {
		texts[i] = l.text
	}
	p.vp.SetContent(strings.Join(texts, "\n"))
	p.vp.GotoBottom()
}

// view is the inside of the panel, inner columns wide and at most height lines high.
func (p logPane) view(st styles, inner, height int) []string {
	if p.fl == nil || height <= 0 {
		return nil
	}
	if len(p.lines) == 0 {
		return []string{paint(st.muted, ansi.Truncate("no logs yet in "+clean(p.fl.dir), inner, "…"))}
	}
	top := min(p.vp.YOffset, len(p.lines))
	shown := p.lines[top:min(top+min(height, p.vp.Height), len(p.lines))]
	out := make([]string, len(shown))
	for i, l := range shown {
		out[i] = ansi.Truncate(logRow(st, l), inner, "…")
	}
	return out
}

// logRow is a line as emit prints it: the source padded by its plain name, a muted │ and the
// colored text.
func logRow(st styles, l logLine) string {
	pad := strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source)))
	return paint(st.sourceStyle(l.source), l.source) + pad + " " + paint(st.muted, "│") + " " + colorLine(st, l.source, l.text)
}
