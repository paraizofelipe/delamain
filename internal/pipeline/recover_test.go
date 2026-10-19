package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/store"
)

const deadPID = 999_999

// crashed seeds an issue run that a dead process left in state at iteration 2, with its worktree.
func crashed(t *testing.T, hs *harness, state store.RunState) store.Run {
	t.Helper()
	return crashedAs(t, hs, state, store.KindIssue, "#7")
}

// crashedAs is crashed for a run of any kind working on ref.
func crashedAs(t *testing.T, hs *harness, state store.RunState, kind store.RunKind, ref string) store.Run {
	t.Helper()
	ctx := context.Background()
	rec, err := hs.st.CreateRun(ctx, "/repo", ref, "Fix add", "omp", 3, kind)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	_ = os.MkdirAll(wt, 0o700)
	rec.State, rec.Iteration, rec.Branch, rec.Worktree, rec.Base, rec.Workspace = state, 2, "fix/7-fix-add", wt, "origin/main", "w5"
	if err := hs.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if ok, err := hs.st.ClaimRun(ctx, rec.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal("seed owner", err)
	}
	hs.g.wt, hs.g.branch = wt, rec.Branch
	hs.r.Alive = func(pid int) bool { return pid != deadPID }
	return rec
}

func recoverAll(t *testing.T, hs *harness) []Result {
	t.Helper()
	out, err := hs.r.Recover(context.Background(), func(string) (forge.Forge, error) { return hs.r.Forge, nil })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRecoverShouldDeliverCommittedWorkAsDraft(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	rec := crashed(t, hs, store.StateImplement)
	fixAdd(hs.g)
	out := recoverAll(t, hs)

	if len(out) != 1 || out[0].Outcome != OutcomeHalted || !strings.Contains(out[0].Reason, "pid 999999") || !strings.Contains(out[0].Reason, "implement") {
		t.Fatalf("results = %+v", out)
	}
	if !slices.Equal(hs.h.closed, []string{"w5"}) {
		t.Fatalf("its restored workspace is closed, closed=%v", hs.h.closed)
	}
	if !slices.Equal(hs.g.pushed, []string{"fix/7-fix-add"}) || len(f.created) != 1 || !f.created[0].Draft || f.created[0].Base != "main" {
		t.Fatalf("committed work becomes a draft PR: pushed=%v created=%+v", hs.g.pushed, f.created)
	}
	last := f.relabels[len(f.relabels)-1]
	if !slices.Equal(last[0], []string{hs.r.Cfg.Forge.Labels.NeedsHuman}) {
		t.Fatalf("issue goes to a human, labels=%v", last)
	}
	got, _ := hs.st.GetRun(context.Background(), rec.ID)
	if got.State != store.StateRecorded || got.OwnerPID != os.Getpid() || got.MRURL == "" {
		t.Fatalf("run must be recorded by the recoverer: %+v", got)
	}
}

func TestRecoverShouldLeaveRunsOfLiveProcessesAlone(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	crashed(t, hs, store.StateImplement)
	hs.r.Alive = func(int) bool { return true }
	if out := recoverAll(t, hs); len(out) != 0 || len(f.created) != 0 || len(hs.h.closed) != 0 {
		t.Fatalf("a running delamain owns its run: %+v", out)
	}
}

func TestRecoverShouldSkipRunsOfThisProcess(t *testing.T) {
	hs := newHarness(t, happy)
	if _, err := hs.st.CreateRun(context.Background(), "/repo", "#7", "x", "omp", 3, store.KindIssue); err != nil {
		t.Fatal(err)
	}
	hs.r.Alive = func(int) bool { return false }
	if out := recoverAll(t, hs); len(out) != 0 {
		t.Fatalf("in-flight runs of this daemon are not crashed: %+v", out)
	}
}

func TestRecoverShouldReuseThePRWhenTheProcessDiedAfterPushing(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	crashed(t, hs, store.StateDeliverDraft)
	fixAdd(hs.g)
	hs.g.remote["fix/7-fix-add"] = true
	f.open = &forge.PR{Number: 5, URL: "https://github.com/acme/api/pull/5", Draft: true}
	out := recoverAll(t, hs)
	if out[0].PRURL != f.open.URL || len(hs.g.pushed) != 0 || len(f.created) != 0 {
		t.Fatalf("the existing PR is the delivery: %+v pushed=%v created=%v", out[0], hs.g.pushed, f.created)
	}
}

func TestRecoverShouldOpenTheMissingPRWhenTheProcessDiedRightAfterPush(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	crashed(t, hs, store.StateDeliver)
	fixAdd(hs.g)
	hs.g.remote["fix/7-fix-add"] = true
	out := recoverAll(t, hs)
	if out[0].PRURL == "" || len(hs.g.pushed) != 0 || len(f.created) != 1 {
		t.Fatalf("push already happened, only the PR is missing: %+v pushed=%v", out[0], hs.g.pushed)
	}
}

func TestRecoverShouldNotDeliverTwice(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	rec := crashed(t, hs, store.StateDeliver)
	rec.MRURL = "https://github.com/acme/api/pull/9"
	_ = hs.st.UpdateRun(context.Background(), rec)
	fixAdd(hs.g)
	out := recoverAll(t, hs)
	if out[0].PRURL != rec.MRURL || len(hs.g.pushed) != 0 || len(f.created) != 0 {
		t.Fatalf("already delivered: %+v", out[0])
	}
}

func TestRecoverShouldDiscardEmptyWork(t *testing.T) {
	hs := newHarness(t, happy)
	rec := crashed(t, hs, store.StatePlan)
	recoverAll(t, hs)
	if !slices.Equal(hs.g.discarded, []string{rec.Branch}) {
		t.Fatalf("a crash during planning leaves nothing behind, discarded=%v", hs.g.discarded)
	}
}

func TestHaltedRunWithoutCommitsShouldRemoveItsWorktreeAndBranch(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{provider: "500 internal"}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !slices.Equal(hs.g.discarded, []string{"fix/7-fix-add"}) || !strings.Contains(res.Delivery, "removidas") {
		t.Fatalf("empty halted work must not block the next attempt: %s discarded=%v delivery=%q", res.Outcome, hs.g.discarded, res.Delivery)
	}
	if !slices.Equal(hs.h.keys["planner-r1"], []string{"close"}) {
		t.Fatal("agents stop before their worktree is removed")
	}
}

func TestHaltedRunShouldKeepWorktreeWhenDiscardIsRefused(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{provider: "500 internal"}
		}
		return happy(role, n, text)
	})
	hs.g.discardErr = errors.New("worktree contains modified or untracked files")
	res := hs.run(t)
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "preservada") {
		t.Fatalf("a refused discard must be reported, warnings=%v", res.Warnings)
	}
}

func TestDeliveredRunShouldKeepItsWorktree(t *testing.T) {
	hs := newHarness(t, happy)
	hs.run(t)
	if len(hs.g.discarded) != 0 {
		t.Fatalf("work with commits is never discarded: %v", hs.g.discarded)
	}
}

func TestRecoverShouldYieldWhenAnotherRecovererClaimsTheRunFirst(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	rec := crashed(t, hs, store.StateImplement)
	fixAdd(hs.g)
	hs.r.Alive = func(pid int) bool {
		// Between listing and claiming, another delamain process takes the run.
		_, _ = hs.st.ClaimRun(context.Background(), rec.ID, deadPID, 4242)
		return false
	}
	if out := recoverAll(t, hs); len(out) != 0 || len(f.created) != 0 || len(hs.g.pushed) != 0 {
		t.Fatalf("a run is finalized by exactly one recoverer: %+v", out)
	}
}

func TestRecoverShouldMarkTheDeadRunsAgentsAsFailed(t *testing.T) {
	hs := newHarness(t, happy)
	rec := crashed(t, hs, store.StateImplement)
	ctx := context.Background()
	_ = hs.st.StartAgent(ctx, store.Agent{RunID: rec.ID, Role: "planner"})
	_ = hs.st.EndAgent(ctx, rec.ID, "planner", store.AgentDone)
	_ = hs.st.StartAgent(ctx, store.Agent{RunID: rec.ID, Role: "implementer", PID: deadPID})
	recoverAll(t, hs)

	agents, _ := hs.st.Agents(ctx, rec.ID)
	if len(agents) != 2 || agents[0].State != store.AgentDone || agents[1].State != store.AgentFailed {
		t.Fatalf("the implementer died with its owner; the planner had finished: %+v", agents)
	}
	if log, _ := os.ReadFile(filepath.Join(hs.r.runDir("/repo", rec.ID), "logs", "delamain.log")); !strings.Contains(string(log), "recovering run") {
		t.Fatalf("the recovery is part of the run's log:\n%s", log)
	}
}

func TestRecoverShouldLeaveARestoredWorkspaceAloneWithoutADisplay(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Display = nil
	crashed(t, hs, store.StateImplement)
	if out := recoverAll(t, hs); len(out) != 1 || len(hs.h.closed) != 0 {
		t.Fatalf("herdr is never called when it is off: %+v closed=%v", out, hs.h.closed)
	}
}

func TestRecoverShouldHaltReviewRunsWithoutDelivering(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	rec := crashedAs(t, hs, store.StateImplement, store.KindReview, "pr:9")
	fixAdd(hs.g)
	ctx := context.Background()
	_ = hs.st.StartAgent(ctx, store.Agent{RunID: rec.ID, Role: "reviewer", PID: deadPID})
	out := recoverAll(t, hs)

	if len(out) != 1 || out[0].Outcome != OutcomeHalted || out[0].Reason != "interrompida: run de review não é retomada" {
		t.Fatalf("results = %+v", out)
	}
	if len(f.created) != 0 || len(f.comments) != 0 || len(f.relabels) != 0 || len(hs.g.pushed) != 0 {
		t.Fatalf("a review run never delivers: created=%v comments=%v relabels=%v pushed=%v", f.created, f.comments, f.relabels, hs.g.pushed)
	}
	if !slices.Contains(hs.g.redproof, "remove") {
		t.Fatalf("the review worktree must be removed, git ops=%v", hs.g.redproof)
	}
	if _, err := os.Stat(rec.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still exists: %v", rec.Worktree, err)
	}
	if len(hs.h.closed) != 0 {
		t.Fatalf("only the worktree and the agents are cleaned up, closed=%v", hs.h.closed)
	}
	agents, _ := hs.st.Agents(ctx, rec.ID)
	if len(agents) != 1 || agents[0].State != store.AgentFailed {
		t.Fatalf("the reviewer died with its owner: %+v", agents)
	}
	got, _ := hs.st.GetRun(ctx, rec.ID)
	if !got.State.Terminal() || !strings.Contains(got.Reason, "interrompida: run de review não é retomada") {
		t.Fatalf("the run must be closed with its reason: %+v", got)
	}
	if left, _ := hs.st.UnfinishedRuns(ctx); len(left) != 0 {
		t.Fatalf("a closed review is not recovered again on the next restart: %+v", left)
	}
}

func TestRecoverShouldKeepFinishingIssueRunsAsBefore(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	crashedAs(t, hs, store.StateImplement, store.KindIssue, "#7")
	fixAdd(hs.g)
	out := recoverAll(t, hs)

	if len(out) != 1 || out[0].Outcome != OutcomeHalted || out[0].PRURL == "" {
		t.Fatalf("results = %+v", out)
	}
	if len(f.created) != 1 || !f.created[0].Draft || !slices.Equal(hs.g.pushed, []string{"fix/7-fix-add"}) {
		t.Fatalf("an issue run keeps being delivered as a draft: created=%+v pushed=%v", f.created, hs.g.pushed)
	}
}
