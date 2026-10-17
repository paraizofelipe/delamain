package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateRunShouldStartClaimedAndBeRetrievable(t *testing.T) {
	s := openTest(t)
	r, err := s.CreateRun(context.Background(), "gh:me/app", "#12", "fix login")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.State != StateClaimed || r.Iteration != 0 {
		t.Fatalf("unexpected new run: %+v", r)
	}
	got, err := s.GetRun(context.Background(), r.ID)
	if err != nil || got.Title != "fix login" {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestCreateRunShouldRefuseSecondActiveRunForSameIssue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "repo", "#1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "repo", "#1", "a"); err == nil {
		t.Fatal("two workers on the same issue would race on one branch")
	}
	if _, err := s.CreateRun(ctx, "other-repo", "#1", "a"); err != nil {
		t.Fatalf("same issue number in another repo is independent: %v", err)
	}
}

func TestCreateRunShouldAllowNewRunAfterPreviousIsRecorded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a")
	r.State = StateRecorded
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "repo", "#1", "a"); err != nil {
		t.Fatalf("a finished issue may be re-run: %v", err)
	}
}

func TestUnfinishedRunsShouldListOnlyResumableRunsInOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.CreateRun(ctx, "repo", "#1", "a")
	b, _ := s.CreateRun(ctx, "repo", "#2", "b")
	c, _ := s.CreateRun(ctx, "repo", "#3", "c")
	b.State = StateNeedsInfo
	c.State, c.Iteration = StateImplement, 2
	_ = s.UpdateRun(ctx, b)
	_ = s.UpdateRun(ctx, c)

	runs, err := s.UnfinishedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != a.ID || runs[1].ID != c.ID || runs[1].Iteration != 2 {
		t.Fatalf("want [#1 claimed, #3 implement iter 2], got %+v", runs)
	}
}

func TestUpdateRunShouldReportMissingRun(t *testing.T) {
	s := openTest(t)
	if err := s.UpdateRun(context.Background(), Run{ID: 999, State: StatePlan}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRecordUsageShouldLedgerOnlyTheDeltaBetweenSnapshots(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a")
	f := "/sessions/implementer/s.jsonl"

	d1, err := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 100, Output: 10, CacheRead: 1000, CostUSD: 0.1})
	if err != nil || d1.Input != 100 {
		t.Fatalf("first snapshot is the whole delta: %+v %v", d1, err)
	}
	d2, _ := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 150, Output: 30, CacheRead: 1500, CostUSD: 0.25})
	if d2.Input != 50 || d2.Output != 20 {
		t.Fatalf("second read must only add the growth: %+v", d2)
	}
	d3, _ := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 150, Output: 30, CacheRead: 1500, CostUSD: 0.25})
	if !d3.zero() {
		t.Fatalf("re-reading an unchanged file must not double count: %+v", d3)
	}

	total, _ := s.RunUsage(ctx, r.ID)
	if total.Input != 150 || total.Output != 30 || total.CacheRead != 1500 {
		t.Fatalf("run total = %+v", total)
	}
	if total.BudgetTokens() != 180 {
		t.Fatalf("budget tokens exclude cacheRead: got %d want 180", total.BudgetTokens())
	}
}

func TestRecordUsageShouldNotCreateNegativeEntriesWhenSnapshotShrinks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a")
	f := "/s.jsonl"
	_, _ = s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 500})
	d, _ := s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 100}) // file rewritten
	if !d.zero() {
		t.Fatalf("shrink must reset baseline, got delta %+v", d)
	}
	d, _ = s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 130})
	if d.Input != 30 {
		t.Fatalf("growth after reset counts from new baseline, got %+v", d)
	}
	total, _ := s.RunUsage(ctx, r.ID)
	if total.Input != 530 {
		t.Fatalf("consumed tokens are never refunded: %+v", total)
	}
}

func TestUsageSinceShouldAggregateAcrossRunsWithinWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	a, _ := s.CreateRun(ctx, "repo", "#1", "a")
	b, _ := s.CreateRun(ctx, "repo", "#2", "b")
	_, _ = s.RecordUsage(ctx, a.ID, "implementer", "/a.jsonl", Usage{Input: 1000}) // yesterday
	clock = time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	_, _ = s.RecordUsage(ctx, a.ID, "implementer", "/a.jsonl", Usage{Input: 1200}) // +200 today
	_, _ = s.RecordUsage(ctx, b.ID, "reviewer", "/b.jsonl", Usage{Input: 300})

	today, err := s.UsageSince(ctx, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if today.Input != 500 {
		t.Fatalf("daily cap must count only today's growth across runs: got %d want 500", today.Input)
	}
}

func TestOpenShouldBeIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s1.CreateRun(ctx, "repo", "#9", "x")
	s1.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen must not re-run migrations: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetRun(ctx, r.ID); err != nil {
		t.Fatalf("state must survive restart: %v", err)
	}
}

func TestOpenShouldRefuseDatabaseFromNewerBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	s, _ := Open(ctx, path)
	_, _ = s.db.ExecContext(ctx, "PRAGMA user_version = 99")
	s.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("silently using a newer schema could corrupt state")
	}
}
