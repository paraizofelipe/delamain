package budget

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

type fixture struct {
	st  *store.Store
	m   Meter
	dir string
}

func newFixture(t *testing.T, cfg config.Budget) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &fixture{st: st, m: Meter{Store: st, Cfg: cfg}, dir: t.TempDir()}
}

func (f *fixture) newRun(t *testing.T, ref string) int64 {
	t.Helper()
	r, err := f.st.CreateRun(context.Background(), "/repo", ref, "t", "omp", 3, store.KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// turn appends one assistant turn to a session file, as omp does.
func turn(t *testing.T, path string, input, output, cacheRead, cacheWrite int64, usd float64) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, `{"type":"message","message":{"role":"assistant","usage":{"input":%d,"output":%d,"cacheRead":%d,"cacheWrite":%d,"cost":{"total":%g}}}}`+"\n",
		input, output, cacheRead, cacheWrite, usd)
}

func tokens(perIssue, daily float64) config.Budget {
	return config.Budget{Mode: config.BudgetTokens, PerIssue: perIssue, Daily: daily, SoftRatio: 0.8}
}

func TestCheckShouldCountAllRolesAndSubagentsExcludingCacheRead(t *testing.T) {
	f := newFixture(t, tokens(1000, 0))
	id := f.newRun(t, "#1")
	turn(t, filepath.Join(f.dir, "planner", "s.jsonl"), 100, 50, 90000, 10, 0.1)
	turn(t, filepath.Join(f.dir, "planner", "s", "Analyze.jsonl"), 200, 0, 0, 0, 0.2)
	turn(t, filepath.Join(f.dir, "reviewer", "r.jsonl"), 40, 0, 0, 0, 0.05)

	s, err := f.m.Check(context.Background(), id, f.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run != 400 || s.Level != OK {
		t.Fatalf("budget tokens = in+out+cacheWrite of every session, got %+v", s)
	}
	if s.Usage.CacheRead != 90000 {
		t.Fatalf("cacheRead is tracked but not budgeted: %+v", s.Usage)
	}
}

func TestCheckShouldNotDoubleCountWhenCalledRepeatedly(t *testing.T) {
	f := newFixture(t, tokens(1000, 0))
	id := f.newRun(t, "#1")
	p := filepath.Join(f.dir, "implementer", "s.jsonl")
	turn(t, p, 100, 0, 0, 0, 0)
	for range 3 {
		if _, err := f.m.Check(context.Background(), id, f.dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	turn(t, p, 50, 0, 0, 0, 0)
	s, _ := f.m.Check(context.Background(), id, f.dir, nil)
	if s.Run != 150 {
		t.Fatalf("watcher ticks re-read the same files: spent must be 150, got %v", s.Run)
	}
}

func TestCheckShouldGoSoftThenHardAtTheIssueLimit(t *testing.T) {
	f := newFixture(t, tokens(1000, 0))
	id := f.newRun(t, "#1")
	p := filepath.Join(f.dir, "implementer", "s.jsonl")
	cases := []struct {
		add  int64
		want Level
	}{{799, OK}, {1, Soft}, {199, Soft}, {1, Hard}}
	for _, c := range cases {
		turn(t, p, c.add, 0, 0, 0, 0)
		s, err := f.m.Check(context.Background(), id, f.dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Level != c.want {
			t.Fatalf("at %v tokens level = %v, want %v", s.Run, s.Level, c.want)
		}
	}
}

func TestCheckShouldEnforceTheDailyCapAcrossRuns(t *testing.T) {
	f := newFixture(t, tokens(0, 500)) // no per-issue limit
	first, second := f.newRun(t, "#1"), f.newRun(t, "#2")
	turn(t, filepath.Join(f.dir, "a", "planner", "s.jsonl"), 450, 0, 0, 0, 0)
	if _, err := f.m.Check(context.Background(), first, filepath.Join(f.dir, "a"), nil); err != nil {
		t.Fatal(err)
	}
	turn(t, filepath.Join(f.dir, "b", "planner", "s.jsonl"), 60, 0, 0, 0, 0)
	s, err := f.m.Check(context.Background(), second, filepath.Join(f.dir, "b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Level != Hard || !strings.Contains(s.Reason, "diário") {
		t.Fatalf("today's spend of all runs counts: %+v", s)
	}
	day, err := f.m.Today(context.Background())
	if err != nil || day.Level != Hard || day.Day != 510 {
		t.Fatalf("preflight view of the day = %+v %v", day, err)
	}
}

func TestCheckShouldUseProviderCostInUSDMode(t *testing.T) {
	f := newFixture(t, config.Budget{Mode: config.BudgetUSD, PerIssue: 1, SoftRatio: 0.8})
	id := f.newRun(t, "#1")
	turn(t, filepath.Join(f.dir, "planner", "s.jsonl"), 999999, 0, 0, 0, 0.85)
	s, _ := f.m.Check(context.Background(), id, f.dir, nil)
	if s.Run != 0.85 || s.Level != Soft || !strings.Contains(s.Left(), "US$") {
		t.Fatalf("usd mode = %+v left=%q", s, s.Left())
	}
}

func TestZeroLimitsShouldDisableEnforcement(t *testing.T) {
	f := newFixture(t, tokens(0, 0))
	id := f.newRun(t, "#1")
	turn(t, filepath.Join(f.dir, "planner", "s.jsonl"), 1e9, 0, 0, 0, 0)
	if s, _ := f.m.Check(context.Background(), id, f.dir, nil); s.Level != OK {
		t.Fatalf("0 means unlimited: %+v", s)
	}
}

func TestLeftShouldTellAgentsWhatRemainsAndWarnWhenSoft(t *testing.T) {
	ok := Status{Mode: config.BudgetTokens, Run: 400_000, PerIssue: 2_000_000, Level: OK}
	if got := ok.Left(); !strings.Contains(got, "1.6M") || !strings.Contains(got, "20%") || strings.Contains(got, "finalize") {
		t.Fatalf("ok left = %q", got)
	}
	soft := Status{Mode: config.BudgetTokens, Run: 1_700_000, PerIssue: 2_000_000, Level: Soft}
	if got := soft.Left(); !strings.Contains(got, "finalize") {
		t.Fatalf("soft limit must tell the agent to wrap up: %q", got)
	}
	if got := (Status{Mode: config.BudgetTokens}).Left(); !strings.Contains(got, "sem limite") {
		t.Fatalf("unlimited = %q", got)
	}
}

func TestFormatShouldBeCompact(t *testing.T) {
	cases := map[float64]string{999: "999", 1500: "1.5K", 167_000: "167K", 2_000_000: "2M", 1_250_000: "1.2M"}
	for v, want := range cases {
		if got := Format(v, config.BudgetTokens); got != want {
			t.Errorf("Format(%v) = %q, want %q", v, got, want)
		}
	}
	if got := Format(0.6712, config.BudgetUSD); got != "US$ 0.67" {
		t.Errorf("usd = %q", got)
	}
}

func TestCheckShouldStopARunWhoseOnlySpendIsStillUnwritten(t *testing.T) {
	f := newFixture(t, tokens(1000, 5000))
	id := f.newRun(t, "#1")
	// omp has not written the implementer's first turn yet: only RPC knows about it.
	live := map[string]store.Usage{"implementer": {Input: 900, Output: 100, CacheRead: 50000, CostUSD: 0.4}}
	s, err := f.m.Check(context.Background(), id, f.dir, live)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run != 1000 || s.Level != Hard || s.Day != 1000 || s.Usage.CostUSD != 0.4 {
		t.Fatalf("live usage must count toward run and day: %+v", s)
	}
}

func TestCheckShouldTakeTheLargerOfFileAndLiveUsagePerRole(t *testing.T) {
	f := newFixture(t, tokens(10000, 0))
	id := f.newRun(t, "#1")
	turn(t, filepath.Join(f.dir, "planner", "s.jsonl"), 300, 0, 0, 0, 0)
	turn(t, filepath.Join(f.dir, "implementer", "s.jsonl"), 200, 0, 0, 0, 0)
	turn(t, filepath.Join(f.dir, "implementer", "s", "Tester.jsonl"), 100, 0, 0, 0, 0)
	live := map[string]store.Usage{
		"planner":     {Input: 250}, // behind the file: nothing extra
		"implementer": {Input: 450}, // main + subagent files hold 300 of it
	}
	s, err := f.m.Check(context.Background(), id, f.dir, live)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run != 750 {
		t.Fatalf("each role counts once, at its larger measure: got %v, want 300+450", s.Run)
	}
}

func TestCheckShouldKeepLiveUsageOutOfTheLedger(t *testing.T) {
	f := newFixture(t, tokens(10000, 0))
	id := f.newRun(t, "#1")
	if _, err := f.m.Check(context.Background(), id, f.dir, map[string]store.Usage{"reviewer": {Input: 500}}); err != nil {
		t.Fatal(err)
	}
	turn(t, filepath.Join(f.dir, "reviewer", "s.jsonl"), 500, 0, 0, 0, 0)
	s, err := f.m.Check(context.Background(), id, f.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Recording the estimate as well would bill the same turn twice once the file lands.
	if s.Run != 500 {
		t.Fatalf("the ledger holds only what session files say: %+v", s)
	}
}
