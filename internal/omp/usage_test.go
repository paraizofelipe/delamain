package omp

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// realUsage is an assistant line captured from a real omp 18.11 session (E2E run 7).
const realUsage = `{"type":"message","message":{"role":"assistant","stopReason":"stop","model":"gpt-5.6-terra","usage":{"input":14686,"output":118,"cacheRead":0,"cacheWrite":0,"totalTokens":14804,"reasoningTokens":31,"cost":{"input":0.029372,"output":0.001416,"cacheRead":0,"cacheWrite":0,"total":0.030788},"premiumRequests":0}}}`

func TestSumUsageShouldAddEveryAssistantTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	body := realUsage + "\n" +
		`{"type":"message","message":{"role":"user","usage":{"input":999}}}` + "\n" + // users carry no billable usage
		`not json` + "\n" +
		`{"type":"message","message":{"role":"assistant","usage":{"input":10,"output":5,"cacheRead":7000,"cacheWrite":3,"cost":{"total":0.5}}}}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	u, err := SumUsage(p)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{Input: 14696, Output: 123, CacheRead: 7000, CacheWrite: 3, CostUSD: 0.530788}
	if u.Input != want.Input || u.Output != want.Output || u.CacheRead != want.CacheRead || u.CacheWrite != want.CacheWrite || u.CostUSD-want.CostUSD > 1e-9 || want.CostUSD-u.CostUSD > 1e-9 {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
}

func TestSumUsageShouldBeZeroForSessionWithoutReplies(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(p, []byte(`{"type":"session","version":3}`+"\n"), 0o600)
	if u, err := SumUsage(p); err != nil || u != (Usage{}) {
		t.Fatalf("got %+v %v", u, err)
	}
}

func TestSessionFilesShouldIncludeSubagentSessions(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"planner/a.jsonl", "planner/a/AnalyzeCalc.jsonl", "reviewer/b.jsonl", "reviewer/b/SpecReview.jsonl", "reviewer/notes.txt"} {
		p := filepath.Join(dir, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, nil, 0o600)
	}
	got, err := SessionFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"planner/a.jsonl", "planner/a/AnalyzeCalc.jsonl", "reviewer/b.jsonl", "reviewer/b/SpecReview.jsonl"}
	for i := range got {
		got[i], _ = filepath.Rel(dir, got[i])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSessionFilesShouldTolerateMissingDir(t *testing.T) {
	if got, err := SessionFiles(filepath.Join(t.TempDir(), "nope")); err != nil || len(got) != 0 {
		t.Fatalf("got %v %v", got, err)
	}
}
