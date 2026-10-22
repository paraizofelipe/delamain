package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

func (f *fixture) logsDir(t *testing.T, rec store.Run) string {
	t.Helper()
	dir := filepath.Join(pipeline.RunDir(f.stateDir, rec.Repo, rec.ID), "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func appendLog(t *testing.T, dir, source, text string) {
	t.Helper()
	fh, err := os.OpenFile(filepath.Join(dir, source+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func sources(out string) []string {
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		src, text, _ := strings.Cut(l, " │ ")
		got = append(got, strings.TrimSpace(src)+":"+text)
	}
	return got
}

func TestLogsShouldInterleaveEverySourceByTime(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateRecorded)
	dir := f.logsDir(t, rec)
	appendLog(t, dir, "delamain", "10:00:00 state run=1 state=plan\n10:00:09 state run=1 state=verify\n")
	appendLog(t, dir, "planner", "10:00:01 ▶ prompt (900 caracteres)\n10:00:05 ■ fim do turno: stop\n")
	appendLog(t, dir, "checks", "10:00:10 ▶ test: go test ./...\nok  ./pkg\n10:00:12 ■ test: código 0\n")
	appendLog(t, dir, "planner", "") // .stderr files and other names never show up
	if err := os.WriteFile(filepath.Join(dir, "planner.stderr"), []byte("secret noise\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := sources(f.exec(t, "logs", "1"))
	want := []string{
		"delamain:10:00:00 state run=1 state=plan",
		"planner:10:00:01 ▶ prompt (900 caracteres)",
		"planner:10:00:05 ■ fim do turno: stop",
		"delamain:10:00:09 state run=1 state=verify",
		"checks:10:00:10 ▶ test: go test ./...",
		"checks:ok  ./pkg", // unstamped gate output stays under its gate
		"checks:10:00:12 ■ test: código 0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLogsShouldFilterByRoleAndKeepOnlyTheTail(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateRecorded)
	dir := f.logsDir(t, rec)
	appendLog(t, dir, "delamain", "10:00:00 a\n")
	appendLog(t, dir, "planner", "10:00:01 b\n")
	appendLog(t, dir, "implementer", "10:00:02 c\n10:00:03 d\n10:00:04 e\n")

	if got := sources(f.exec(t, "logs", "1", "--role", "planner,implementer")); strings.Join(got, ",") != "planner:10:00:01 b,implementer:10:00:02 c,implementer:10:00:03 d,implementer:10:00:04 e" {
		t.Fatalf("role filter: %v", got)
	}
	if got := sources(f.exec(t, "logs", "1", "--tail", "2")); strings.Join(got, ",") != "implementer:10:00:03 d,implementer:10:00:04 e" {
		t.Fatalf("tail: %v", got)
	}
}

func TestLogsShouldSayWhenARunHasNoLogs(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "", store.StateRecorded)
	if out := f.exec(t, "logs", "1"); !strings.HasPrefix(out, "no logs in ") {
		t.Fatalf("got %q", out)
	}
	if out, err := execute(t, "--config", f.cfgFile, "logs", "7"); err == nil {
		t.Fatalf("an unknown run is an error: %s", out)
	}
}

// syncBuffer is read by the test while the command writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLogsFollowShouldStreamNewLinesAndStopWhenTheRunEnds(t *testing.T) {
	old := followInterval
	followInterval = 5 * time.Millisecond
	t.Cleanup(func() { followInterval = old })
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement)
	dir := f.logsDir(t, rec)
	appendLog(t, dir, "delamain", "10:00:00 state run=1 state=implement\n")

	out := &syncBuffer{}
	errc := make(chan error, 1)
	go func() {
		root := NewRoot()
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs([]string{"--config", f.cfgFile, "logs", "1", "-f"})
		errc <- root.Execute()
	}()
	waitFor(t, func() bool { return strings.Contains(out.String(), "state=implement") }, "the existing lines")

	// A role that starts later, and a line written in two pieces.
	appendLog(t, dir, "implementer", "10:00:05 ▶ pro")
	appendLog(t, dir, "implementer", "mpt\n")
	waitFor(t, func() bool { return strings.Contains(out.String(), "implementer │ 10:00:05 ▶ prompt\n") }, "a role's log appearing mid-run")
	if strings.Count(out.String(), "▶ pro") != 1 {
		t.Fatalf("a partial line is printed once, when complete:\n%s", out)
	}

	appendLog(t, dir, "delamain", "10:00:09 state run=1 state=recorded\n")
	rec.State = store.StateRecorded
	if err := f.st.UpdateRun(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f must end by itself once the run is finished")
	}
	if !strings.HasSuffix(out.String(), "state=recorded\n") {
		t.Fatalf("the last lines are never lost when the run ends:\n%s", out)
	}
}

func TestLogFollowerShouldStartOverWhenAFileIsTruncated(t *testing.T) {
	dir := t.TempDir()
	fl := &logFollower{dir: dir, offsets: map[string]int64{}, partial: map[string][]byte{}, lastTS: map[string]string{}}
	appendLog(t, dir, "checks", "10:00:00 a long first line\n")
	if lines, _ := fl.read(false); len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks.log"), []byte("10:01:00 new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _ := fl.read(false)
	if len(lines) != 1 || lines[0].text != "10:01:00 new" {
		t.Fatalf("a rewritten file is read from the start: %+v", lines)
	}
}

// sampleLogs writes a line of every kind for each known source and one unknown source.
func sampleLogs(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{
		"delamain":    "10:00:00 gate run=1 name=format exit=0\n10:00:09 WARN verify failed run=1\n",
		"checks":      "10:00:01 ▶ test: go test ./...\nok  \tgithub.com/x/agent\t0.003s\n?   \tgithub.com/x/cmd\t[no test files]\n10:00:02 ■ test: código 1\n",
		"planner":     "10:00:03 ▶ prompt (900 caracteres)\n",
		"implementer": "10:00:04 🔧 Bash: go vet ./...\n10:00:05 ⑂ ✗ Bash falhou (exit 1)\n",
		"reviewer":    "10:00:06 💬 ok\n",
		"other":       "10:00:07 something\n",
	}
	for source, text := range files {
		appendLog(t, dir, source, text)
	}
	return files
}

const sampleLogsOutput = `delamain    │ 10:00:00 gate run=1 name=format exit=0
checks      │ 10:00:01 ▶ test: go test ./...
checks      │ ok  	github.com/x/agent	0.003s
checks      │ ?   	github.com/x/cmd	[no test files]
checks      │ 10:00:02 ■ test: código 1
planner     │ 10:00:03 ▶ prompt (900 caracteres)
implementer │ 10:00:04 🔧 Bash: go vet ./...
implementer │ 10:00:05 ⑂ ✗ Bash falhou (exit 1)
reviewer    │ 10:00:06 💬 ok
other       │ 10:00:07 something
delamain    │ 10:00:09 WARN verify failed run=1
`

func TestLogsShouldNotEmitANSIWhenNotATerminal(t *testing.T) {
	f := newFixture(t)
	sampleLogs(t, f.logsDir(t, f.run(t, "#1", "", store.StateRecorded)))
	colorEnv(t, "", "")
	if out := f.exec(t, "logs", "1"); out != sampleLogsOutput {
		t.Fatalf("a writer that is not a terminal gets the plain lines, byte for byte:\n%q\nwant\n%q", out, sampleLogsOutput)
	}
}

func TestLogsShouldNotEmitANSIWhenNoColorIsSet(t *testing.T) {
	f := newFixture(t)
	sampleLogs(t, f.logsDir(t, f.run(t, "#1", "", store.StateRecorded)))
	colorEnv(t, "1", "1") // NO_COLOR wins over CLICOLOR_FORCE
	for _, args := range [][]string{{"logs", "1"}, {"logs", "1", "--tail", "3"}} {
		out := f.exec(t, args...)
		if strings.Contains(out, "\x1b") || !strings.HasSuffix(sampleLogsOutput, out) {
			t.Fatalf("%v: NO_COLOR prints the plain lines:\n%q", args, out)
		}
	}
}

func TestLogsShouldColorEachSourceName(t *testing.T) {
	f := newFixture(t)
	sampleLogs(t, f.logsDir(t, f.run(t, "#1", "", store.StateRecorded)))
	colorEnv(t, "1", "")
	out := f.exec(t, "logs", "1")
	for source, color := range map[string]string{"delamain": "35", "checks": "33", "planner": "34", "implementer": "36", "reviewer": "32"} {
		wantCodes(t, out, source, source, "1", color)
	}
	wantCodes(t, out, "other", "other", "1") // an unknown source is only bold
	wantCodes(t, out, "other", "  ")         // the padding stays out of the style
	for _, prefix := range []string{"delamain    │ 10:00:00", "planner", "other", "checks      │ ok"} {
		wantCodes(t, out, prefix, "│", "2")
	}
	for prefix, stamp := range map[string]string{"delamain": "10:00:00", "checks      │ 10:00:01": "10:00:01", "planner": "10:00:03", "implementer": "10:00:04", "reviewer": "10:00:06", "other": "10:00:07"} {
		wantCodes(t, out, prefix, stamp, "2")
	}
	wantCodes(t, out, "other", "something")
}

func TestLogsShouldKeepTheSeparatorAlignedWithColor(t *testing.T) {
	f := newFixture(t)
	sampleLogs(t, f.logsDir(t, f.run(t, "#1", "", store.StateRecorded)))
	colorEnv(t, "1", "")
	out := f.exec(t, "logs", "1")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("a forced color colors the logs:\n%q", out)
	}
	if got := stripANSI(out); got != sampleLogsOutput {
		t.Errorf("without the escapes the colored logs are the plain ones, tabs included:\n%q\nwant\n%q", got, sampleLogsOutput)
	}
}

func TestLogsShouldLeaveTheLogFilesUntouchedWithColor(t *testing.T) {
	f := newFixture(t)
	dir := f.logsDir(t, f.run(t, "#1", "", store.StateRecorded))
	files := sampleLogs(t, dir)
	colorEnv(t, "1", "")
	if out := f.exec(t, "logs", "1"); !strings.Contains(out, "\x1b[") {
		t.Fatalf("a forced color colors the logs:\n%q", out)
	}
	for source, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, source+".log"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s.log is only read: %q, want %q", source, got, want)
		}
	}
}

func TestLogsFollowShouldColorLinesAsTheyArrive(t *testing.T) {
	old := followInterval
	followInterval = 5 * time.Millisecond
	t.Cleanup(func() { followInterval = old })
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement)
	dir := f.logsDir(t, rec)
	appendLog(t, dir, "delamain", "10:00:00 state run=1 state=implement\n")
	colorEnv(t, "1", "")

	out := &syncBuffer{}
	errc := make(chan error, 1)
	go func() {
		root := NewRoot()
		root.SetOut(out)
		root.SetErr(out)
		root.SetArgs([]string{"--config", f.cfgFile, "logs", "1", "-f"})
		errc <- root.Execute()
	}()
	waitFor(t, func() bool { return strings.Contains(stripANSI(out.String()), "state=implement") }, "the existing lines")

	appendLog(t, dir, "implementer", "10:00:05 🔧 Bash: go")
	appendLog(t, dir, "implementer", " vet\n")
	waitFor(t, func() bool {
		return strings.Contains(stripANSI(out.String()), "implementer │ 10:00:05 🔧 Bash: go vet\n")
	}, "a line written after the start")
	if strings.Count(stripANSI(out.String()), "🔧") != 1 {
		t.Fatalf("a partial line is printed once, when complete:\n%q", out)
	}
	wantCodes(t, out.String(), "implementer", "implementer", "1", "36")
	wantCodes(t, out.String(), "implementer", "│", "2")

	appendLog(t, dir, "reviewer", "10:00:08 💬 unterminated") // flushed, colored, once the run ends
	rec.State = store.StateRecorded
	if err := f.st.UpdateRun(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logs -f must end by itself once the run is finished")
	}
	if !strings.HasSuffix(stripANSI(out.String()), "reviewer    │ 10:00:08 💬 unterminated\n") {
		t.Fatalf("the partial line comes out when the run ends:\n%q", out)
	}
	wantCodes(t, out.String(), "reviewer", "reviewer", "1", "32")
}

// lineStyles are the looks of `logs` in a terminal with the given color profile.
func lineStyles(p termenv.Profile) styles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(p)
	return newStyles(r)
}

// sampleLines has a line of every kind each source writes, and lines crafted to trip the parse.
var sampleLines = map[string][]string{
	"delamain": {
		"05:27:48 state run=7 state=verify iteration=1",
		"05:27:48 gate run=7 name=format exit=0",
		"05:27:48 gate run=7 name=test exit=1",
		"05:27:48 gate run=7 name=test exit=x",
		"05:31:38 agent stopped run=7 role=reviewer state=done",
		"05:31:44 delivered run=7 url=https://github.com/o/r/pull/32 draft=false",
		"05:27:58 WARN verify failed run=7 err=\"a b=c\"",
		"05:27:58 ERROR run failed run=7 err=\"boom\"",
		"05:27:58 note run=7 err=\"unterminated",
		"05:27:58 message only",
		"05:27:58 tab\there run=7 v=a\tb",
		"not a stamped line state=halted",
	},
	"checks": {
		"05:27:49 ▶ test: go test ./...",
		"05:27:58 ■ test: código 0",
		"05:27:58 ■ test: código 2",
		"05:27:58 ■ test: estourou o tempo",
		"ok  \tgithub.com/x/agent\t0.003s",
		"FAIL\tgithub.com/x/forge\t0.412s",
		"--- FAIL: TestX (0.00s)",
		"?   \tgithub.com/x/cmd\t[no test files]",
		"okay, not a result",
		"\tsome\toutput\r",
		"",
	},
	"implementer": {
		"05:04:29 ▶ prompt (900 caracteres)",
		"05:04:30 🔧 Bash: go vet ./internal/forge/...",
		"05:04:48 ✗ Bash falhou (exit 1): \tx",
		"05:05:05 💬 Transição commitada (`98b5581`).",
		"05:05:06 ⑂ subagent explore: mapa",
		"05:05:07 ⑂ ✗ falhou",
		"05:05:08 ⑂ 🔧 Read: a.go",
		"05:05:09 ⑂ 💬 achei",
		"05:05:10 ■ fim do turno: stop · in 1 out 2 cacheW 3",
		"05:05:11 ⚠ rate limit",
		"continuation of a 💬",
		"05:05:12 \xff invalid utf-8",
	},
	"other": {"05:05:13 something"},
}

func TestColorLineShouldReturnTheTextUnchangedWithoutColor(t *testing.T) {
	plain, forced := lineStyles(termenv.Ascii), lineStyles(termenv.ANSI)
	for source, lines := range sampleLines {
		for _, text := range lines {
			if got := colorLine(plain, source, text); got != text {
				t.Errorf("%s: without colors the line is unchanged: %q, want %q", source, got, text)
			}
			if got := stripANSI(colorLine(forced, source, text)); got != text {
				t.Errorf("%s: colors add escapes, never rewrite bytes: %q, want %q", source, got, text)
			}
		}
	}
}

func TestColorLineShouldColorTheOrchestrator(t *testing.T) {
	st := lineStyles(termenv.ANSI)
	line := func(text string) string { return colorLine(st, "delamain", text) }

	out := line("05:31:47 state run=9 state=recorded")
	wantCodes(t, out, "", "05:31:47", "2")
	wantCodes(t, out, "", "state", "1") // the message
	wantCodes(t, out, "", "run=", "2")
	wantCodes(t, out, "", "9")
	wantCodes(t, out, "", "state=", "2")
	wantCodes(t, out, "", "recorded", "32")
	wantCodes(t, line("05:31:47 state run=7 state=halted"), "", "halted", "31")
	wantCodes(t, line("05:31:47 state run=7 state=done"), "", "done", "36")                       // a run state
	wantCodes(t, line("05:31:38 agent stopped run=7 role=reviewer state=done"), "", "done", "32") // an agent state
	wantCodes(t, line("05:31:38 agent stopped run=7 role=reviewer state=failed"), "", "failed", "31")

	wantCodes(t, line("22:22:22 gate run=7 name=format exit=0"), "", "0", "32")
	wantCodes(t, line("22:22:22 gate run=7 name=test exit=1"), "", "1", "31")
	wantCodes(t, line("22:22:22 gate run=7 name=test exit=n/a"), "", "n/a") // not a code

	out = line("05:31:44 delivered run=7 url=https://github.com/o/r/pull/32 draft=false")
	wantCodes(t, out, "", "delivered", "1")
	wantCodes(t, out, "", "https://github.com/o/r/pull/32", "36", "4")
	wantCodes(t, out, "", "draft=", "2")
	wantCodes(t, out, "", "false")

	out = line(`05:27:58 WARN verify failed run=7 err="a b=c"`)
	wantCodes(t, out, "", "WARN", "1", "33")
	wantCodes(t, out, "", `verify failed run=7 err="a b=c"`, "33")
	out = line(`05:27:58 ERROR run failed run=7 err="boom"`)
	wantCodes(t, out, "", "ERROR", "1", "31")
	wantCodes(t, out, "", `run failed run=7 err="boom"`, "31")

	out = line(`05:27:58 retry run=7 err="a b=c" n=2`) // a quoted value is not split at its spaces
	wantCodes(t, out, "", "retry", "1")
	wantCodes(t, out, "", `"a b=c"`)
	wantCodes(t, out, "", "n=", "2")
	wantCodes(t, line("05:27:58 message only"), "", "message only", "1")
	wantCodes(t, line("state run=7 state=halted"), "", "halted") // no stamp: not an orchestrator line
}
