package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartSessionShouldKeepEveryIterationOfARole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	other, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)

	start := func(a AgentSession) int64 {
		t.Helper()
		id, err := s.StartSession(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(time.Minute)
		return id
	}
	planner := start(AgentSession{RunID: r.ID, Role: "planner", Model: "opus"})
	first := start(AgentSession{RunID: r.ID, Role: "implementer", Iteration: 1, Model: "sonnet"})
	start(AgentSession{RunID: other.ID, Role: "implementer", Iteration: 1, Model: "haiku"})
	second := start(AgentSession{RunID: r.ID, Role: "implementer", Iteration: 2, Model: "opus",
		State: AgentDone, StartedAt: clock.Add(time.Hour), EndedAt: clock.Add(time.Hour)})

	got, err := s.Sessions(ctx, []int64{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[other.ID]; ok || len(got) != 1 {
		t.Fatalf("only the runs asked for come back: %+v", got)
	}
	ss := got[r.ID]
	if len(ss) != 3 || ss[0].ID != planner || ss[1].ID != first || ss[2].ID != second {
		t.Fatalf("one session per start, in start order: %+v", ss)
	}
	base := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	want := []struct {
		role      string
		iteration int
		model     string
		started   time.Time
	}{
		{"planner", 0, "opus", base},
		{"implementer", 1, "sonnet", base.Add(time.Minute)},
		{"implementer", 2, "opus", base.Add(3 * time.Minute)},
	}
	for i, w := range want {
		a := ss[i]
		if a.RunID != r.ID || a.Role != w.role || a.Iteration != w.iteration || a.Model != w.model ||
			a.State != AgentRunning || !a.StartedAt.Equal(w.started) || !a.EndedAt.IsZero() {
			t.Fatalf("session %d = %+v, want %+v running since %v", i, a, w, w.started)
		}
	}
}

func TestSessionsShouldReturnAnEmptyMapWithoutRuns(t *testing.T) {
	s := openTest(t)
	got, err := s.Sessions(context.Background(), nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no runs, no sessions, but a usable map: %#v %v", got, err)
	}
}

func TestEndSessionShouldRecordStateAndEnd(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	first, _ := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "implementer", Iteration: 1})
	clock = clock.Add(time.Minute)
	if _, err := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "reviewer", Iteration: 1}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)

	if err := s.EndSession(ctx, first, AgentFailed); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Sessions(ctx, []int64{r.ID})
	ss := got[r.ID]
	if len(ss) != 2 {
		t.Fatalf("sessions = %+v", ss)
	}
	end := time.Date(2026, 10, 29, 9, 2, 0, 0, time.UTC)
	if ss[0].ID != first || ss[0].State != AgentFailed || !ss[0].EndedAt.Equal(end) {
		t.Fatalf("the ended session keeps its state and end: %+v", ss[0])
	}
	if ss[1].State != AgentRunning || !ss[1].EndedAt.IsZero() {
		t.Fatalf("the other session is still running: %+v", ss[1])
	}
}

func TestEndSessionShouldReportAMissingSession(t *testing.T) {
	s := openTest(t)
	if err := s.EndSession(context.Background(), 42, AgentDone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending an unknown session = %v, want ErrNotFound", err)
	}
}

func TestSessionsShouldSplitTheLedgerBetweenSessionsOfTheSameRole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	other, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)
	at := func(minute int) { clock = time.Date(2026, 10, 29, 9, minute, 0, 0, time.UTC) }
	record := func(runID int64, role, file string, u Usage) {
		t.Helper()
		if _, err := s.RecordUsage(ctx, runID, role, file, u); err != nil {
			t.Fatal(err)
		}
	}

	at(0)
	planner, _ := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "planner"})
	at(1)
	record(r.ID, "implementer", "/s/before", Usage{Input: 1000, CostUSD: 8})
	at(2)
	first, _ := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "implementer", Iteration: 1})
	at(3)
	record(r.ID, "implementer", "/s/first", Usage{Input: 100, Output: 10, CacheRead: 1000, CacheWrite: 50, CostUSD: 1})
	record(r.ID, "planner", "/s/planner", Usage{Input: 7, CostUSD: 0.25})
	record(other.ID, "implementer", "/s/other", Usage{Input: 5000, CostUSD: 16})
	at(4)
	record(r.ID, "implementer", "/s/first", Usage{Input: 300, Output: 30, CacheRead: 3000, CacheWrite: 150, CostUSD: 3})
	at(5)
	second, _ := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "implementer", Iteration: 2})
	record(r.ID, "implementer", "/s/second", Usage{Input: 40, CostUSD: 0.5})
	at(6)
	record(r.ID, "implementer", "/s/second-sub", Usage{Input: 2, CostUSD: 0.25})

	got, err := s.Sessions(ctx, []int64{r.ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]Usage{
		planner: {Input: 7, CostUSD: 0.25},
		first:   {Input: 300, Output: 30, CacheRead: 3000, CacheWrite: 150, CostUSD: 3},
		second:  {Input: 42, CostUSD: 0.75},
	}
	if len(got[r.ID]) != 3 {
		t.Fatalf("sessions = %+v", got)
	}
	for _, a := range got[r.ID] {
		if a.Usage != want[a.ID] {
			t.Fatalf("%s iteration %d used %+v, want %+v", a.Role, a.Iteration, a.Usage, want[a.ID])
		}
	}
}

func TestSessionsShouldKeepUsageRecordedAfterTheSessionEnded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	impl, _ := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "implementer", Iteration: 1})
	clock = clock.Add(10 * time.Minute)
	if err := s.EndSession(ctx, impl, AgentDone); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if _, err := s.StartSession(ctx, AgentSession{RunID: r.ID, Role: "reviewer", Iteration: 1}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if _, err := s.RecordUsage(ctx, r.ID, "implementer", "/s/impl", Usage{Input: 500, Output: 20, CostUSD: 2}); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Sessions(ctx, []int64{r.ID})
	ss := got[r.ID]
	if len(ss) != 2 || ss[0].ID != impl {
		t.Fatalf("sessions = %+v", ss)
	}
	if want := (Usage{Input: 500, Output: 20, CostUSD: 2}); ss[0].Usage != want {
		t.Fatalf("the meter bills the session after it ends: %+v, want %+v", ss[0].Usage, want)
	}
	if !ss[1].Usage.zero() {
		t.Fatalf("another role keeps none of it: %+v", ss[1].Usage)
	}
}
