package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/pipeline"
)

// followInterval paces `logs -f`; tests shorten it.
var followInterval = 250 * time.Millisecond

func newLogsCmd(load func() (config.Config, error)) *cobra.Command {
	var follow bool
	var roles []string
	var tail int
	cmd := &cobra.Command{
		Use:   "logs RUN_ID",
		Short: "Show a run's logs: orchestrator, gates and each omp agent",
		Long: `Interleaves, by each line's timestamp, the orchestrator log (delamain), the gates output
(checks) and each omp agent's log (planner, implementer, reviewer). With -f it follows the run
live and exits on its own once the run finishes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			id, err := runID(args[0])
			if err != nil {
				return err
			}
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			rec, err := in.st.GetRun(ctx, id)
			if err != nil {
				return fmt.Errorf("run %d: %w", id, err)
			}
			fl := &logFollower{dir: filepath.Join(pipeline.RunDir(in.cfg.Paths.StateDir, rec.Repo, rec.ID), "logs"), roles: roles}
			finished := func() bool {
				r, err := in.st.GetRun(ctx, id)
				return err != nil || r.State.Terminal()
			}
			return fl.run(ctx, c.OutOrStdout(), follow, tail, finished)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the run live until it finishes (Ctrl+C exits earlier)")
	cmd.Flags().StringSliceVar(&roles, "role", nil, "only these sources: delamain, checks, planner, implementer, reviewer")
	cmd.Flags().IntVar(&tail, "tail", 0, "only the last N lines already written (0: all)")
	return cmd
}

// logFollower reads a run's logs/<source>.log files incrementally.
type logFollower struct {
	dir     string
	roles   []string
	offsets map[string]int64  // source -> bytes already read
	partial map[string][]byte // source -> trailing line not yet terminated
	lastTS  map[string]string // source -> time of its last stamped line
}

type logLine struct{ ts, source, text string }

var stampRe = regexp.MustCompile(`^\d\d:\d\d:\d\d `)

func (fl *logFollower) run(ctx context.Context, w io.Writer, follow bool, tail int, finished func() bool) error {
	fl.offsets, fl.partial, fl.lastTS = map[string]int64{}, map[string][]byte{}, map[string]string{}
	lines, err := fl.read(false)
	if err != nil {
		return err
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if len(lines) == 0 && !follow {
		_, err := fmt.Fprintf(w, "no logs in %s\n", fl.dir)
		return err
	}
	st := newStyles(lipgloss.NewRenderer(w))
	if err := emit(w, st, lines); err != nil || !follow {
		return err
	}
	t := time.NewTicker(followInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		end := finished() // checked before reading, so the final lines are never lost
		lines, err := fl.read(end)
		if err != nil {
			return err
		}
		if err := emit(w, st, lines); err != nil {
			return err
		}
		if end {
			return nil
		}
	}
}

// read returns the lines appended since the last call, ordered by their time stamp. flush also
// returns unterminated last lines (the run has ended, nothing more will complete them).
func (fl *logFollower) read(flush bool) ([]logLine, error) {
	paths, err := filepath.Glob(filepath.Join(fl.dir, "*.log"))
	if err != nil {
		return nil, err
	}
	var out []logLine
	for _, p := range paths {
		source := strings.TrimSuffix(filepath.Base(p), ".log")
		if len(fl.roles) > 0 && !slices.Contains(fl.roles, source) {
			continue
		}
		chunk, err := fl.readFrom(p, source)
		if err != nil {
			return nil, err
		}
		data := append(fl.partial[source], chunk...)
		cut := bytes.LastIndexByte(data, '\n') + 1
		if flush {
			cut = len(data)
		}
		fl.partial[source] = slices.Clone(data[cut:])
		if cut == 0 {
			continue
		}
		for _, text := range strings.Split(strings.TrimSuffix(string(data[:cut]), "\n"), "\n") {
			if stampRe.MatchString(text) {
				fl.lastTS[source] = text[:8]
			}
			// A line without a stamp (gate output) belongs with the stamped line before it.
			out = append(out, logLine{ts: fl.lastTS[source], source: source, text: text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out, nil
}

func (fl *logFollower) readFrom(path, source string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a log of the run dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < fl.offsets[source] { // truncated or recreated: start over
		fl.offsets[source], fl.partial[source] = 0, nil
	}
	if _, err := f.Seek(fl.offsets[source], io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	fl.offsets[source] += int64(len(data))
	return data, err
}

// emit prints each line as "%-11s │ %s"; the padding counts the plain source name, so colors
// never shift the separator.
func emit(w io.Writer, st styles, lines []logLine) error {
	for _, l := range lines {
		pad := strings.Repeat(" ", max(0, 11-utf8.RuneCountInString(l.source)))
		if _, err := fmt.Fprintf(w, "%s%s %s %s\n", paint(st.sourceStyle(l.source), l.source), pad, paint(st.muted, "│"), colorLine(st, l.source, l.text)); err != nil {
			return err
		}
	}
	return nil
}

// colorLine styles one complete log line of source; without colors it returns text unchanged.
func colorLine(st styles, source, text string) string {
	if !stampRe.MatchString(text) {
		if source == "checks" {
			return colorGateOutput(st, text)
		}
		return text
	}
	stamp, rest := paint(st.muted, text[:8])+" ", text[9:]
	switch source {
	case "delamain":
		return stamp + colorOrchestrator(st, rest)
	case "checks":
		return stamp + colorGate(st, rest)
	case "planner", "implementer", "reviewer":
		return stamp + colorAgent(st, rest)
	}
	return stamp + rest
}

// colorAgent styles an agent line by its marker, which "⑂ " precedes in a subagent's lines;
// what the agent says (💬) and unknown markers are left as is.
func colorAgent(st styles, rest string) string {
	if marked(rest, "⑂ subagent") {
		return paint(st.muted, rest)
	}
	body := strings.TrimPrefix(rest, "⑂ ")
	sub := rest[:len(rest)-len(body)]
	switch {
	case marked(body, "▶ prompt"):
		prompt, size := paint(st.none.Bold(true), "▶ prompt"), body[len("▶ prompt"):]
		if strings.HasPrefix(size, " (") {
			return sub + prompt + " " + paint(st.muted, size[1:])
		}
		return sub + prompt + size
	case marked(body, "🔧"), marked(body, "■ fim do turno:"):
		return paint(st.muted, rest)
	case marked(body, "✗"):
		return paint(st.bad, rest)
	}
	return rest
}

// marked reports whether s starts with marker followed by a space, or is only the marker.
func marked(s, marker string) bool { return s == marker || strings.HasPrefix(s, marker+" ") }

// gateEndRe matches "■ <gate>: código <n>" as pipeline/checks.go writes it; the ó is escaped
// because this package's literals are English text (language_test.go).
var gateEndRe = regexp.MustCompile(`^■ [^:]+: c\x{f3}digo (-?\d+)$`)

// colorGate styles the "▶ <gate>: <command>" and "■ <gate>: código <n>" lines of the gates.
func colorGate(st styles, rest string) string {
	if strings.HasPrefix(rest, "▶ ") {
		if i := strings.IndexByte(rest, ':'); i >= 0 {
			return paint(st.none.Bold(true), rest[:i+1]) + rest[i+1:]
		}
		return rest
	}
	if m := gateEndRe.FindStringSubmatch(rest); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil && code == 0 {
			return paint(st.ok, rest)
		}
		return paint(st.bad, rest)
	}
	return rest
}

// colorGateOutput styles the results in the raw output of a gate's command, as go test prints
// them; any other line is left as is.
func colorGateOutput(st styles, text string) string {
	if startsWord(text, "ok") {
		return paint(st.ok, "ok") + text[len("ok"):]
	}
	for _, prefix := range []string{"", "--- "} {
		if after, ok := strings.CutPrefix(text, prefix); ok && startsWord(after, "FAIL") {
			return prefix + paint(st.bad.Bold(true), "FAIL") + after[len("FAIL"):]
		}
	}
	if strings.HasPrefix(text, "?") && strings.HasSuffix(text, "[no test files]") {
		parts := strings.Split(text, "\t") // the tabs stay raw, as paint never renders them
		for i, p := range parts {
			parts[i] = paint(st.muted, p)
		}
		return strings.Join(parts, "\t")
	}
	return text
}

// startsWord reports whether s starts with the whole word w.
func startsWord(s, w string) bool {
	after, ok := strings.CutPrefix(s, w)
	return ok && (after == "" || strings.ContainsRune(" \t:", rune(after[0])))
}

// attrRe finds the " key=" that starts each attribute of an orchestrator line.
var attrRe = regexp.MustCompile(` [A-Za-z_][A-Za-z0-9_.]*=`)

// colorOrchestrator styles "[LEVEL ]msg k=v k=v"; what does not parse is left as is.
func colorOrchestrator(st styles, rest string) string {
	for level, sty := range map[string]lipgloss.Style{"WARN": st.warn, "ERROR": st.bad} {
		if after, ok := strings.CutPrefix(rest, level+" "); ok {
			return paint(sty.Bold(true), level) + " " + paint(sty, after)
		}
	}
	loc := attrRe.FindStringIndex(rest)
	if loc == nil {
		return paint(st.none.Bold(true), rest)
	}
	msg, attrs := rest[:loc[0]], rest[loc[0]:]
	var b strings.Builder
	b.WriteString(paint(st.none.Bold(true), msg))
	for attrs != "" {
		m := attrRe.FindStringIndex(attrs)
		if m == nil || m[0] != 0 {
			break
		}
		value, tail, ok := attrValue(attrs[m[1]:])
		if !ok {
			break
		}
		key := attrs[1:m[1]]
		b.WriteString(" " + paint(st.muted, key) + paint(st.valueStyle(msg, key, value), value))
		attrs = tail
	}
	return b.String() + attrs
}

// attrValue splits the value that starts s, quoted as the orchestrator log quotes it, from the
// attributes after it.
func attrValue(s string) (value, tail string, ok bool) {
	if strings.HasPrefix(s, `"`) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil || (len(q) < len(s) && s[len(q)] != ' ') {
			return "", "", false
		}
		return q, s[len(q):], true
	}
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i:], true
	}
	return s, "", true
}

// valueStyle colors the values that tell how a run is going; the others stay plain.
func (st styles) valueStyle(msg, key, value string) lipgloss.Style {
	switch key {
	case "state=":
		if msg == "agent stopped" {
			return st.agentStateStyle(value)
		}
		return st.runStateStyle(value)
	case "exit=":
		if code, err := strconv.Atoi(value); err == nil {
			if code == 0 {
				return st.ok
			}
			return st.bad
		}
	case "url=":
		return st.info.Underline(true)
	}
	return st.none
}

// paint renders s only when lipgloss keeps its bytes: it rewrites tabs and line breaks, and
// ranges over the runes of an underlined text. Anything else is printed as is.
func paint(sty lipgloss.Style, s string) string {
	if s == "" || strings.ContainsAny(s, "\t\r\n") || !utf8.ValidString(s) {
		return s
	}
	return sty.Render(s)
}
