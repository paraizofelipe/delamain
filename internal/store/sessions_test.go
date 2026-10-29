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
