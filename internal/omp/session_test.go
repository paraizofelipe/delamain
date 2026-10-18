package omp

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lines mirror the shape captured in the M0 spike (values synthetic).
const (
	header       = `{"type":"session","version":3,"id":"s1","cwd":"/wt"}`
	userMsg      = `{"type":"message","message":{"role":"user","content":"do it"}}`
	assistantOK  = `{"type":"message","message":{"role":"assistant","stopReason":"stop","model":"gpt-5.6-terra","usage":{"input":10}}}`
	assistantErr = `{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":"401 Authentication Error","model":"labs-code-max","usage":{"input":0}}}`
)

func session(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLastAssistantShouldExposeProviderFailureThatHerdrReportsAsDone(t *testing.T) {
	a, err := LastAssistant(session(t, header, userMsg, assistantErr))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Failed() || !strings.Contains(a.ErrorMessage, "401") {
		t.Fatalf("provider error must be visible: %+v", a)
	}
}

func TestLastAssistantShouldUseTheLatestReplyOnly(t *testing.T) {
	a, err := LastAssistant(session(t, header, userMsg, assistantErr, userMsg, assistantOK))
	if err != nil {
		t.Fatal(err)
	}
	if a.Failed() || a.Model != "gpt-5.6-terra" {
		t.Fatalf("a recovered session must not be flagged: %+v", a)
	}
}

func TestLastAssistantShouldReportSessionWithoutReply(t *testing.T) {
	if _, err := LastAssistant(session(t, header, userMsg, "garbage line")); !errors.Is(err, ErrNoAssistant) {
		t.Fatalf("want ErrNoAssistant, got %v", err)
	}
}

func TestLatestSessionShouldPickNewestTopLevelFileAndIgnoreSubagents(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "a.jsonl")
	newer := filepath.Join(dir, "b.jsonl")
	sub := filepath.Join(dir, "b", "Analyzer.jsonl")
	for _, p := range []string{older, newer, sub} {
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(header), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(older, past, past)
	got, err := LatestSession(dir)
	if err != nil || got != newer {
		t.Fatalf("got %q %v, want %q", got, err, newer)
	}
	if _, err := LatestSession(t.TempDir()); err == nil {
		t.Fatal("empty dir has no session")
	}
}
