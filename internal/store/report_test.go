package store

import (
	"context"
	"slices"
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

func TestRunSummariesShouldFilterByPeriodRepoAndKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	day1 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	clock := day1
	s.now = func() time.Time { return clock }
	old, _ := s.CreateRun(ctx, "acme/a", "#1", "a", "omp", 3, 0, KindIssue)
	clock = day2
	review, _ := s.CreateRun(ctx, "acme/a", "#2", "b", "omp", 3, 0, KindReview)
	clock = day2.Add(time.Minute)
	other, _ := s.CreateRun(ctx, "acme/b", "#3", "c", "omp", 3, 0, KindIssue)

	// Since is honoured whatever its zone: 15:00 at UTC+3 is day2 at 12:00 UTC.
	since := day2.In(time.FixedZone("UTC+3", 3*60*60))
	for _, tc := range []struct {
		name string
		f    ReportFilter
		want []int64
	}{
		{"no filter keeps every run", ReportFilter{}, []int64{other.ID, review.ID, old.ID}},
		{"since", ReportFilter{Since: since}, []int64{other.ID, review.ID}},
		{"repo", ReportFilter{Repo: "acme/a"}, []int64{review.ID, old.ID}},
		{"kind", ReportFilter{Kind: string(KindReview)}, []int64{review.ID}},
		{"all together", ReportFilter{Since: since, Repo: "acme/b", Kind: string(KindIssue)}, []int64{other.ID}},
		{"no match", ReportFilter{Since: since, Repo: "acme/a", Kind: string(KindIssue)}, nil},
	} {
		got, err := s.RunSummaries(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var ids []int64
		for _, r := range got {
			ids = append(ids, r.Run.ID)
		}
		if !slices.Equal(ids, tc.want) {
			t.Errorf("%s: got runs %v, want %v", tc.name, ids, tc.want)
		}
	}
}

// useLocal makes loc the machine zone for the test; call it before openTest, the test must not be parallel.
func useLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	prev := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = prev })
}

func TestDailyCostByRoleShouldGroupByLocalDay(t *testing.T) {
	// At UTC-3, 23:30 and 00:30 local fall on the same UTC day but on different local days.
	brt := time.FixedZone("UTC-3", -3*60*60)
	useLocal(t, brt)
	s := openTest(t)
	ctx := context.Background()
	var clock time.Time
	s.now = func() time.Time { return clock.UTC() }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	record := func(at time.Time, role, file string, cost float64) {
		t.Helper()
		clock = at
		if _, err := s.RecordUsage(ctx, r.ID, role, file, Usage{Input: 1, CostUSD: cost}); err != nil {
			t.Fatal(err)
		}
	}
	record(time.Date(2026, 10, 19, 23, 0, 0, 0, brt), "planner", "/old.jsonl", 8)      // before Since
	record(time.Date(2026, 10, 20, 22, 0, 0, 0, brt), "implementer", "/i.jsonl", 0.25) // 01:00 UTC on the 21st
	record(time.Date(2026, 10, 20, 23, 30, 0, 0, brt), "planner", "/p1.jsonl", 0.5)    // 02:30 UTC on the 21st
	record(time.Date(2026, 10, 21, 0, 30, 0, 0, brt), "planner", "/p2.jsonl", 1)       // 03:30 UTC on the 21st
	record(time.Date(2026, 10, 21, 0, 45, 0, 0, brt), "planner", "/p3.jsonl", 0.25)

	got, err := s.DailyCostByRole(ctx, ReportFilter{Since: time.Date(2026, 10, 20, 0, 0, 0, 0, brt)})
	if err != nil {
		t.Fatal(err)
	}
	want := []DayCost{
		{Day: time.Date(2026, 10, 20, 0, 0, 0, 0, brt), Role: "implementer", CostUSD: 0.25},
		{Day: time.Date(2026, 10, 20, 0, 0, 0, 0, brt), Role: "planner", CostUSD: 0.5},
		{Day: time.Date(2026, 10, 21, 0, 0, 0, 0, brt), Role: "planner", CostUSD: 1.25},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].Day.Equal(want[i].Day) || got[i].Role != want[i].Role || got[i].CostUSD != want[i].CostUSD {
			t.Fatalf("row %d: got %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}
