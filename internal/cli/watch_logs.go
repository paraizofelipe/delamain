package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// logSources are the chips of the logs panel, picked with the keys 1 to 6; the first shows them all.
var logSources = []string{"all", "delamain", "checks", "planner", "implementer", "reviewer"}

// logLook is how the logs panel fits its layout.
type logLook int

const (
	logsFull    logLook = iota // separators, chips and footer in full
	logsShort                  // no separators, chips cut to 4 letters, a short footer
	logsLetters                // logsShort with a letter for each source and the time in HH:MM
)

// shortSources are the chips of logsShort, letterSources those of logsLetters.
var (
	shortSources  = []string{"all", "delm", "chec", "plan", "impl", "revi"}
	letterSources = []string{"all", "D", "C", "P", "I", "R"}
)

// maxLogLines, maxLogLineBytes and maxLogReadBytes bound the memory of the logs panel: the
// lines it holds, the start of each line it keeps, and what one reload reads of each file.
// Older lines are dropped and counted.
const (
	maxLogLines     = 5000
	maxLogLineBytes = 4 << 10
	maxLogReadBytes = 1 << 20
)

// logPane is the logs panel of `watch`: the lines read so far of the selected run and the
// viewport over them. Update only swaps fl; its fields are touched by the reload command alone.
type logPane struct {
	fl     *logFollower // nil without a selected run
	run    int64        // the run fl reads
	lines  []logLine    // control characters already dropped
	omit   int          // lines dropped to keep maxLogLines
	source int          // index in logSources; it outlives reset
	shown  []logLine    // the lines of source
	paused bool         // the user scrolled away: new lines do not move the view
	vp     viewport.Model
}

// reset points the pane at the logs of run, in dir, and forgets what was read; it does no I/O.
func (p *logPane) reset(run int64, dir string) {
	p.fl, p.run, p.lines, p.omit, p.paused = nil, run, nil, 0, false
	if dir != "" {
		p.fl = &logFollower{dir: dir, offsets: map[string]int64{}, partial: map[string][]byte{}, lastTS: map[string]string{}}
	}
	p.vp = viewport.New(0, p.vp.Height)
	p.refresh()
}

// append adds lines read from the files after skipped ones the reload did not read: their text
// is from third parties, so it is cut to maxLogLineBytes and loses its control characters here.
func (p *logPane) append(lines []logLine, skipped int) {
	if skipped > 0 { // what is held is older than the lines skipped
		p.omit += len(p.lines) + skipped
		p.lines = nil
	}
	if drop := len(lines) - maxLogLines; drop > 0 {
		p.omit += drop
		lines = lines[drop:]
	}
	for _, l := range lines {
		// Copies: a cut text or a stamp would keep the whole text of the reload in memory.
		p.lines = append(p.lines, logLine{ts: strings.Clone(l.ts), source: strings.Clone(clean(l.source)), text: strings.Clone(clean(cutBytes(l.text)))})
	}
	if drop := len(p.lines) - maxLogLines; drop > 0 {
		p.omit += drop
		p.lines = append([]logLine(nil), p.lines[drop:]...) // lets go of the dropped ones
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

// resize gives the log lines rows lines of the pane; it does nothing when they already have them.
func (p *logPane) resize(rows int) {
	if p.vp.Height == rows {
		return
	}
	p.vp.Height = rows
	p.refresh()
}

// paneRows splits height between the separators and the log lines: the log lines give way
// first, then the separators, so the chips and the footer stay. Only logsFull has separators.
func paneRows(height int, look logLook) (seps, rows int) {
	if look != logsFull {
		return 0, max(height-2, 0)
	}
	return min(max(height-2, 0), 2), max(height-4, 0)
}

// refresh gives the viewport the lines of the source and keeps it at the end.
func (p *logPane) refresh() {
	p.shown = nil // a new array: copies of the model share the old one
	for _, l := range p.lines {
		if p.source == 0 || l.source == logSources[p.source] {
			p.shown = append(p.shown, l)
		}
	}
	n := len(p.shown)
	if p.omit > 0 {
		n++ // the omitted line opens the content
	}
	// The viewport only scrolls: view renders the lines, so it gets as many empty ones.
	p.vp.SetContent(strings.Repeat("\n", max(n-1, 0)))
	if !p.paused {
		p.vp.GotoBottom()
	}
	p.vp.SetYOffset(p.vp.YOffset) // within the content, which a filter may have shortened
}

// cutBytes keeps the first maxLogLineBytes of s, whole runes only.
func cutBytes(s string) string {
	if len(s) <= maxLogLineBytes {
		return s
	}
	n := maxLogLineBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// skipBacklog moves fl past what exceeds maxLogReadBytes in each file, so its next read stays
// bounded, and returns how many lines it skipped; the line it lands in is read from there on.
func skipBacklog(fl *logFollower) (int, error) {
	paths, err := filepath.Glob(filepath.Join(fl.dir, "*.log"))
	if err != nil {
		return 0, err
	}
	skipped := 0
	for _, path := range paths {
		source := strings.TrimSuffix(filepath.Base(path), ".log")
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		from := fl.offsets[source]
		if info.Size() < from { // truncated: read starts over
			from = 0
		}
		to := info.Size() - maxLogReadBytes
		if to <= from {
			continue
		}
		n, err := countLines(path, from, to)
		if err != nil {
			return 0, err
		}
		skipped += n
		fl.offsets[source], fl.partial[source] = to, nil
	}
	return skipped, nil
}

// countLines counts the line breaks between the offsets from and to of path.
func countLines(path string, from, to int64) (int, error) {
	f, err := os.Open(path) //nolint:gosec // a log of the run dir
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, buf := 0, make([]byte, 32<<10)
	r := io.NewSectionReader(f, from, to-from)
	for {
		k, err := r.Read(buf)
		n += bytes.Count(buf[:k], []byte{'\n'})
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// capPartial keeps the start of each unterminated line of fl, as append keeps of a line.
func capPartial(fl *logFollower) {
	for source, b := range fl.partial {
		if len(b) > maxLogLineBytes {
			fl.partial[source] = bytes.Clone(b[:maxLogLineBytes])
		}
	}
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
func (p logPane) view(st styles, run RunView, inner, height int, look logLook) []string {
	if p.fl == nil || height <= 0 {
		return nil
	}
	seps, rows := paneRows(height, look)
	rows = min(rows, p.vp.Height)
	sep := paint(st.muted, strings.Repeat("─", max(inner, 0)))
	out := []string{ansi.Truncate(p.chips(st, look), inner, "…")}
	if seps > 0 {
		out = append(out, sep)
	}
	var body []string
	if look == logsFull {
		body = p.cutRows(st, inner, rows)
	} else {
		body = p.wrappedRows(st, inner, rows, look)
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
	return append(out, p.footer(st, run, inner, look))[:min(len(out)+1, height)]
}

// cutRows are the log lines from the top of the viewport that fill rows lines, each cut to
// inner columns.
func (p logPane) cutRows(st styles, inner, rows int) []string {
	body := []string{}
	if len(p.lines) == 0 && rows > 0 {
		body = append(body, paint(st.muted, ansi.Truncate("no logs yet in "+clean(p.fl.dir), inner, "…")))
	}
	top := p.vp.YOffset
	if p.omit > 0 {
		if top == 0 && rows > 0 {
			body = append(body, paint(st.muted, ansi.Truncate(fmt.Sprintf("… %d earlier lines omitted", p.omit), inner, "…")))
		}
		top-- // the omitted line comes before the first one
	}
	top = min(max(top, 0), len(p.shown))
	for _, l := range p.shown[top:max(top, min(top+rows-len(body), len(p.shown)))] {
		body = append(body, ansi.Truncate(logRow(st, l), inner, "…"))
	}
	return body
}

// wrappedRows are the log lines wrapped to inner columns that fill rows lines: the last ones
// when following, the ones from the top of the viewport when paused. Only the lines that show
// are wrapped: a line may be thousands of columns long and there may be thousands of them.
func (p logPane) wrappedRows(st styles, inner, rows int, look logLook) []string {
	if rows <= 0 {
		return nil
	}
	if len(p.lines) == 0 {
		return wrapStyled(paint(st.muted, "no logs yet in "+clean(p.fl.dir)), inner)
	}
	omitted := func() []string {
		return wrapStyled(paint(st.muted, fmt.Sprintf("… %d earlier lines omitted", p.omit)), inner)
	}
	var out []string
	if !p.paused {
		for i := len(p.shown) - 1; i >= 0 && len(out) < rows; i-- {
			out = append(wrapRow(st, p.shown[i], inner, look), out...)
		}
		if len(out) < rows && p.omit > 0 {
			out = append(omitted(), out...)
		}
		return out[max(len(out)-rows, 0):]
	}
	top := p.vp.YOffset
	if p.omit > 0 {
		if top == 0 {
			out = omitted()
		}
		top-- // the omitted line comes before the first one
	}
	for i := max(top, 0); i < len(p.shown) && len(out) < rows; i++ {
		out = append(out, wrapRow(st, p.shown[i], inner, look)...)
	}
	return out[:min(len(out), rows)]
}

// wrapRow is logRow wrapped to inner columns: the continuations start under the text, past the
// source, the │ and the time, which are not repeated. logsLetters shows the source as a letter
// and the time without its seconds.
func wrapRow(st styles, l logLine, inner int, look logLook) []string {
	source, stamp := l.source+strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source))), 8
	if look == logsLetters {
		source, stamp = sourceLetter(l.source), 5
	}
	prefix := paint(st.sourceStyle(l.source), source) + " " + paint(st.muted, "│") + " "
	text := colorLine(st, l.source, l.text)
	if stampRe.MatchString(l.text) {
		prefix += paint(st.muted, l.text[:stamp]) + " "
		text = colorRest(st, l.source, l.text[9:])
	}
	indent := lipgloss.Width(prefix)
	rows := wrapStyled(text, inner-indent)
	for i := range rows {
		if i == 0 {
			rows[i] = prefix + rows[i]
		} else {
			rows[i] = strings.Repeat(" ", indent) + rows[i]
		}
	}
	return rows
}

// sourceLetter is the letter of a log source in logsLetters: its chip's, or its first one.
func sourceLetter(source string) string {
	for i, s := range logSources {
		if s == source && i > 0 {
			return letterSources[i]
		}
	}
	r, _ := utf8.DecodeRuneInString(source)
	return strings.ToUpper(string(r))
}

// chips name the sources to filter by; the active one is in reverse video, in its color.
func (p logPane) chips(st styles, look logLook) string {
	out, names := paint(st.muted, "source")+" ", logSources
	switch look {
	case logsShort:
		out, names = "", shortSources
	case logsLetters:
		out, names = "", letterSources
	}
	for i, s := range names {
		color := st.sourceStyle(logSources[i])
		if i == 0 {
			color = st.none
		}
		if i == p.source {
			out += paint(color.Reverse(true), "["+s+"]")
		} else {
			out += paint(color, " "+s+" ")
		}
	}
	return out
}

// footer tells where the logs stand at the left and how many lines are shown at the right.
func (p logPane) footer(st styles, run RunView, inner int, look logLook) string {
	follow, pause, count := "following the end · G", "paused · G back to the end", "%d of %d lines · %d%%"
	if look != logsFull {
		follow, pause, count = "following · G", "paused · G", "%d/%d · %d%%"
	}
	left := paint(st.muted, follow)
	switch {
	case run.terminal:
		left = paint(st.muted, "ended · ") + resultCell(st, run.Result).styled
	case p.paused:
		left = paint(st.muted, pause)
	}
	right := paint(st.muted, fmt.Sprintf(count, len(p.shown), len(p.lines), int(math.Round(p.vp.ScrollPercent()*100))))
	gap := inner - lipgloss.Width(left) - lipgloss.Width(right)
	return ansi.Truncate(left+strings.Repeat(" ", max(gap, 1))+right, inner, "…")
}

// logRow is a line as emit prints it: the source padded by its plain name, a muted │ and the
// colored text.
func logRow(st styles, l logLine) string {
	pad := strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source)))
	return paint(st.sourceStyle(l.source), l.source) + pad + " " + paint(st.muted, "│") + " " + colorLine(st, l.source, l.text)
}
