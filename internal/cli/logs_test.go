package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestLogsShouldReportNoLogsInEnglish(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "", store.StateRecorded)
	if out := f.exec(t, "logs", "1"); !strings.HasPrefix(out, "no logs in ") {
		t.Fatalf("got %q", out)
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
