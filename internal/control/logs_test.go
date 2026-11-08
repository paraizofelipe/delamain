package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func appendLog(t *testing.T, dir, source, text string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(filepath.Join(dir, source+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestLogReaderShouldInterleaveSourcesByStampAndCarryTheStampToUnstampedLines(t *testing.T) {
	dir := t.TempDir()
	appendLog(t, dir, "delamain", "10:00:00 gate run=1 name=format exit=0\n10:00:09 WARN verify failed run=1\n")
	appendLog(t, dir, "checks", "10:00:01 ▶ test: go test ./...\nok  \tgithub.com/x/agent\t0.003s\n10:00:02 ■ test: código 1\n")
	appendLog(t, dir, "planner", "10:00:03 ▶ prompt\n")
	lines, err := NewLogReader(dir, nil).Read(false)
	if err != nil {
		t.Fatal(err)
	}
	want := []LogLine{
		{"10:00:00", "delamain", "10:00:00 gate run=1 name=format exit=0"},
		{"10:00:01", "checks", "10:00:01 ▶ test: go test ./..."},
		{"10:00:01", "checks", "ok  \tgithub.com/x/agent\t0.003s"},
		{"10:00:02", "checks", "10:00:02 ■ test: código 1"},
		{"10:00:03", "planner", "10:00:03 ▶ prompt"},
		{"10:00:09", "delamain", "10:00:09 WARN verify failed run=1"},
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines =\n%v\nwant\n%v", lines, want)
	}
	only, err := NewLogReader(dir, []string{"planner", "checks"}).Read(false)
	if err != nil || len(only) != 4 || only[0].Source != "checks" || only[3].Source != "planner" {
		t.Fatalf("roles filter the sources: %v %v", only, err)
	}
}

func TestLogReaderShouldHoldAPartialLineUntilItCompletesOrTheRunEnds(t *testing.T) {
	dir := t.TempDir()
	r := NewLogReader(dir, nil)
	appendLog(t, dir, "implementer", "10:00:05 ▶ pro")
	if lines, _ := r.Read(false); len(lines) != 0 {
		t.Fatalf("an unterminated line waits: %v", lines)
	}
	appendLog(t, dir, "implementer", "mpt\n10:00:06 tail")
	lines, _ := r.Read(false)
	if len(lines) != 1 || lines[0].Text != "10:00:05 ▶ prompt" {
		t.Fatalf("the completed line comes out once, whole: %v", lines)
	}
	lines, _ = r.Read(true)
	if len(lines) != 1 || lines[0].Text != "10:00:06 tail" || lines[0].TS != "10:00:06" {
		t.Fatalf("flush returns the unterminated line: %v", lines)
	}
	if lines, _ := r.Read(true); len(lines) != 0 {
		t.Fatalf("nothing is returned twice: %v", lines)
	}
}

func TestLogReaderShouldStartOverWhenAFileIsTruncated(t *testing.T) {
	dir := t.TempDir()
	r := NewLogReader(dir, nil)
	appendLog(t, dir, "checks", "10:00:00 a long first line\n")
	if lines, _ := r.Read(false); len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks.log"), []byte("10:01:00 new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, _ := r.Read(false)
	if len(lines) != 1 || lines[0].Text != "10:01:00 new" {
		t.Fatalf("a rewritten file is read from the start: %+v", lines)
	}
}

func TestCursorShouldRoundTripThroughAnOpaqueStringAndResumeAReader(t *testing.T) {
	dir := t.TempDir()
	appendLog(t, dir, "delamain", "10:00:00 one\n10:00:01 two\n")
	appendLog(t, dir, "checks", "10:00:00 ▶ test\npartial")
	r := NewLogReader(dir, nil)
	if _, err := r.Read(false); err != nil {
		t.Fatal(err)
	}
	c := r.Cursor()
	enc := c.Encode()
	if enc == "" || strings.ContainsAny(enc, "+/=") {
		t.Fatalf("the cursor is a non-empty base64url string: %q", enc)
	}
	back, err := ParseCursor(enc)
	if err != nil {
		t.Fatal(err)
	}
	if back.Offsets["delamain"] != 26 || back.Offsets["checks"] != 25 || string(back.Partial["checks"]) != "partial" || back.LastTS["checks"] != "10:00:00" {
		t.Fatalf("the cursor keeps offsets, the partial line and the stamps: %+v", back)
	}

	appendLog(t, dir, "checks", " line\n")
	appendLog(t, dir, "delamain", "10:00:02 three\n")
	resumed := NewLogReader(dir, nil)
	resumed.Seek(back)
	lines, err := resumed.Read(false)
	if err != nil {
		t.Fatal(err)
	}
	want := []LogLine{{"10:00:00", "checks", "partial line"}, {"10:00:02", "delamain", "10:00:02 three"}}
	if !slices.Equal(lines, want) {
		t.Fatalf("a reader resumed from the cursor continues where the other stopped:\n%v\nwant\n%v", lines, want)
	}
	if (Cursor{}).Encode() != "" {
		t.Fatal("an empty cursor encodes as the empty string")
	}
	if c, err := ParseCursor(""); err != nil || len(c.Offsets) != 0 {
		t.Fatalf("the empty string is the start: %+v %v", c, err)
	}
}

func TestParseCursorShouldRejectWhatEncodeNeverWrote(t *testing.T) {
	for name, s := range map[string]string{
		"not base64":    "%%%",
		"not json":      "bm90IGpzb24",
		"unknown field": "eyJ4IjoxfQ",                         // {"x":1}
		"negative":      "eyJvIjp7ImRlbGFtYWluIjotMX19",       // {"o":{"delamain":-1}}
		"path source":   "eyJvIjp7Ii4uL2V0Yy9wYXNzd2QiOjF9fQ", // {"o":{"../etc/passwd":1}}
		"bad stamp":     "eyJ0Ijp7ImRlbGFtYWluIjoibm9wZSJ9fQ", // {"t":{"delamain":"nope"}}
		"dot source":    "eyJwIjp7Ii4uIjoiWVE9PSJ9fQ",         // {"p":{"..":"YQ=="}}
	} {
		if _, err := ParseCursor(s); err == nil || !strings.Contains(err.Error(), "invalid log cursor") {
			t.Errorf("%s: %q must be rejected as an invalid cursor, got %v", name, s, err)
		}
	}
}

func TestLogReaderShouldSkipTheBacklogAndCapPartialLines(t *testing.T) {
	dir := t.TempDir()
	r := NewLogReader(dir, nil)
	appendLog(t, dir, "checks", strings.Repeat("10:00:00 x\n", 100)+"10:00:01 last\n")
	skipped, err := r.SkipBacklog(20)
	if err != nil || skipped != 99 {
		t.Fatalf("the lines beyond the last 20 bytes are skipped and counted: %d %v", skipped, err)
	}
	lines, _ := r.Read(false)
	if len(lines) != 2 || lines[1].Text != "10:00:01 last" {
		t.Fatalf("the read goes on from the line it landed in: %v", lines)
	}
	appendLog(t, dir, "checks", strings.Repeat("y", 100))
	if _, err := r.Read(false); err != nil {
		t.Fatal(err)
	}
	r.CapPartial(10)
	if got := r.Cursor().Partial["checks"]; len(got) != 10 {
		t.Fatalf("the partial line is capped: %d bytes", len(got))
	}
}
