package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentsShouldTrackEachRoleFromStartToEnd(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)

	if err := s.StartAgent(ctx, Agent{RunID: r.ID, Role: "planner", PID: 41, LogPath: "/l/planner.log", SessionDir: "/s/planner"}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := s.StartAgent(ctx, Agent{RunID: r.ID, Role: "implementer", PID: 42}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := s.EndAgent(ctx, r.ID, "planner", AgentDone); err != nil {
		t.Fatal(err)
	}

	got, err := s.Agents(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "planner" || got[1].Role != "implementer" {
		t.Fatalf("agents come in start order: %+v", got)
	}
	p, i := got[0], got[1]
	if p.State != AgentDone || p.PID != 41 || p.LogPath != "/l/planner.log" || p.SessionDir != "/s/planner" ||
		!p.EndedAt.Equal(clock) || !p.StartedAt.Equal(clock.Add(-2*time.Minute)) {
		t.Fatalf("planner = %+v", p)
	}
	if i.State != AgentRunning || !i.EndedAt.IsZero() {
		t.Fatalf("an agent that never ended is still running: %+v", i)
	}
}

func TestStartAgentShouldReplaceAnEarlierSessionOfTheSameRole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	_ = s.StartAgent(ctx, Agent{RunID: r.ID, Role: "implementer", PID: 1})
	_ = s.EndAgent(ctx, r.ID, "implementer", AgentFailed)
	if err := s.StartAgent(ctx, Agent{RunID: r.ID, Role: "implementer", PID: 2}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Agents(ctx, r.ID)
	if len(got) != 1 || got[0].PID != 2 || got[0].State != AgentRunning || !got[0].EndedAt.IsZero() {
		t.Fatalf("one row per role, showing the live session: %+v", got)
	}
}

func TestEndAgentShouldReportAnAgentThatNeverStarted(t *testing.T) {
	s := openTest(t)
	r, _ := s.CreateRun(context.Background(), "repo", "#1", "a", "omp", 3, 0, KindIssue)
	if err := s.EndAgent(context.Background(), r.ID, "reviewer", AgentDone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestFailRunningAgentsShouldOnlyTouchAgentsStillMarkedRunning(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	other, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)
	_ = s.StartAgent(ctx, Agent{RunID: r.ID, Role: "planner"})
	_ = s.EndAgent(ctx, r.ID, "planner", AgentDone)
	_ = s.StartAgent(ctx, Agent{RunID: r.ID, Role: "implementer"})
	_ = s.StartAgent(ctx, Agent{RunID: other.ID, Role: "implementer"})

	n, err := s.FailRunningAgents(ctx, r.ID)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := s.Agents(ctx, r.ID)
	if got[0].State != AgentDone || got[1].State != AgentFailed || got[1].EndedAt.IsZero() {
		t.Fatalf("a finished agent keeps its state; the orphan fails: %+v", got)
	}
	if o, _ := s.Agents(ctx, other.ID); o[0].State != AgentRunning {
		t.Fatalf("another run's agents are untouched: %+v", o)
	}
}

func TestRoleUsageShouldSumTheLedgerPerRole(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	other, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, 0, KindIssue)
	_, _ = s.RecordUsage(ctx, r.ID, "implementer", "/a.jsonl", Usage{Input: 100})
	_, _ = s.RecordUsage(ctx, r.ID, "implementer", "/a.jsonl", Usage{Input: 150, Output: 10})
	_, _ = s.RecordUsage(ctx, r.ID, "implementer", "/a/Sub.jsonl", Usage{Input: 5})
	_, _ = s.RecordUsage(ctx, r.ID, "planner", "/p.jsonl", Usage{CacheWrite: 7, CostUSD: 0.5})
	_, _ = s.RecordUsage(ctx, other.ID, "planner", "/o.jsonl", Usage{Input: 999})

	got, err := s.RoleUsage(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["implementer"] != (Usage{Input: 155, Output: 10}) || got["planner"] != (Usage{CacheWrite: 7, CostUSD: 0.5}) {
		t.Fatalf("usage per role = %+v", got)
	}
	if empty, err := s.RoleUsage(ctx, 999); err != nil || len(empty) != 0 {
		t.Fatalf("a run without usage has none: %+v %v", empty, err)
	}
}

func TestRecentRunsShouldListNewestFirstUpToTheLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, ref := range []string{"#1", "#2", "#3"} {
		if _, err := s.CreateRun(ctx, "repo", ref, ref, "omp", 3, 0, KindIssue); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RecentRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].IssueRef != "#3" || got[1].IssueRef != "#2" {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenShouldAddTheAgentsTableToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delamain.db")
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, 0, KindIssue)
	_ = s.StartAgent(ctx, Agent{RunID: r.ID, Role: "planner", PID: 7})
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _ := s.Agents(ctx, r.ID); len(got) != 1 || got[0].PID != 7 {
		t.Fatalf("agents survive a restart: %+v", got)
	}
}
