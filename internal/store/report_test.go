package store

import (
	"context"
	"testing"
	"time"
)

func TestRunSummariesShouldSumTheLedgerOfEachRun(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	a, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	clock = clock.Add(time.Hour)
	b, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)
	_, _ = s.RecordUsage(ctx, a.ID, "planner", "/a-p.jsonl", Usage{Input: 100, CostUSD: 0.5})
	_, _ = s.RecordUsage(ctx, a.ID, "implementer", "/a-i.jsonl", Usage{Output: 20, CacheRead: 3, CostUSD: 0.25})
	_, _ = s.RecordUsage(ctx, b.ID, "reviewer", "/b.jsonl", Usage{Input: 7, CacheWrite: 9, CostUSD: 1})

	got, err := s.RunSummaries(ctx, ReportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Run.ID != b.ID || got[1].Run.ID != a.ID {
		t.Fatalf("want runs newest first [%d %d], got %+v", b.ID, a.ID, got)
	}
	if want := (Usage{Input: 7, CacheWrite: 9, CostUSD: 1}); got[0].Usage != want {
		t.Fatalf("usage of %s = %+v, want %+v", got[0].Run.IssueRef, got[0].Usage, want)
	}
	if want := (Usage{Input: 100, Output: 20, CacheRead: 3, CostUSD: 0.75}); got[1].Usage != want {
		t.Fatalf("usage of %s = %+v, want %+v", got[1].Run.IssueRef, got[1].Usage, want)
	}
	if got[1].Run.Repo != "repo" || got[1].Run.Kind != KindIssue || !got[1].Run.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("the run comes whole: %+v", got[1].Run)
	}
}

func TestRunSummariesShouldKeepARunWithoutUsage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	used, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	clock = clock.Add(time.Hour)
	idle, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)
	_, _ = s.RecordUsage(ctx, used.ID, "planner", "/a.jsonl", Usage{Input: 10, CostUSD: 0.5})

	got, err := s.RunSummaries(ctx, ReportFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Run.ID != idle.ID || got[0].Usage != (Usage{}) {
		t.Fatalf("a run without usage is listed with zero usage: %+v", got)
	}
	if got[1].Run.ID != used.ID || got[1].Usage != (Usage{Input: 10, CostUSD: 0.5}) {
		t.Fatalf("the other run keeps its usage: %+v", got[1])
	}
}
