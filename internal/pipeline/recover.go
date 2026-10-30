package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// Recover finalizes runs whose delamain process died (crash, reboot, kill): their agents are
// stopped, committed work is delivered as a draft, empty work is removed and the run is recorded
// as halted. It never resumes agent work: a half-finished turn is not trustworthy.
// Runs that do not work an issue (review, address) are only cleaned up and halted, never delivered.
// forgeFor resolves each run's repository forge; nil keeps delivery local.
func (r *Runner) Recover(ctx context.Context, forgeFor func(repo string) (forge.Forge, error)) ([]Result, error) {
	runs, err := r.Store.UnfinishedRuns(ctx)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, rec := range runs {
		res, err := r.recoverOrphan(ctx, rec, forgeFor)
		if errors.Is(err, errNotOrphaned) {
			continue // still being driven by a live process, or another one is recovering it
		}
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// RecoverRun is Recover for run id alone. It fails for a run that does not exist, already
// finished or is still driven by a live process.
func (r *Runner) RecoverRun(ctx context.Context, id int64, forgeFor func(repo string) (forge.Forge, error)) (Result, error) {
	rec, err := r.Store.GetRun(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("run %d: %w", id, err)
	}
	if rec.State.Terminal() {
		return Result{}, fmt.Errorf("run %d already finished (%s)", id, rec.State)
	}
	res, err := r.recoverOrphan(ctx, rec, forgeFor)
	if errors.Is(err, errNotOrphaned) {
		return res, fmt.Errorf("run %d: %w", id, err)
	}
	return res, err
}

// errNotOrphaned means a run's owner is alive or another process claimed it first.
var errNotOrphaned = errors.New("the run is not orphaned: its owner is alive or another process is recovering it")

// recoverOrphan claims a run whose owner died and finalizes it.
func (r *Runner) recoverOrphan(ctx context.Context, rec store.Run, forgeFor func(repo string) (forge.Forge, error)) (Result, error) {
	alive := r.Alive
	if alive == nil {
		alive = ProcessAlive
	}
	if rec.OwnerPID == os.Getpid() || alive(rec.OwnerPID) {
		return Result{}, errNotOrphaned
	}
	claimed, err := r.Store.ClaimRun(ctx, rec.ID, rec.OwnerPID, os.Getpid())
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{}, errNotOrphaned
	}
	rr := *r
	rr.Forge = nil
	if forgeFor != nil && rec.Kind == store.KindIssue {
		fg, err := forgeFor(rec.Repo)
		if err != nil {
			r.Log.Warn("recover: no forge, delivering locally", "run", rec.ID, "err", err)
		} else {
			rr.Forge = fg
		}
	}
	res, err := rr.recoverRun(ctx, rec)
	if err != nil {
		return res, fmt.Errorf("recover run %d: %w", rec.ID, err)
	}
	return res, nil
}

func (r *Runner) recoverRun(ctx context.Context, rec store.Run) (Result, error) {
	who := fmt.Sprintf("pid %d", rec.OwnerPID)
	if rec.OwnerPID == 0 {
		who = "pid desconhecido"
	}
	res := Result{
		RunID: rec.ID, Outcome: OutcomeHalted, Branch: rec.Branch, Base: rec.Base, Worktree: rec.Worktree,
		WorkspaceID: rec.Workspace, Iterations: rec.Iteration, PRURL: rec.MRURL,
		Complexity: rec.Complexity,
		Reason:     fmt.Sprintf("run interrompida: o processo do delamain (%s) parou no estado %s", who, rec.State),
	}
	t := Task{Ref: rec.IssueRef, Title: rec.Title, Kind: kindFromBranch(rec.Branch), Repo: rec.Repo, Issue: issueNumber(rec.IssueRef)}
	day, err := r.meter().Today(ctx)
	if err != nil {
		return res, err
	}
	maxIter, perIssue := rec.Limits(r.Cfg.Pipeline.MaxIterations, r.Cfg.Budget.PerIssue)
	ru := &run{
		rec: rec, task: prompts.Issue{Ref: t.Ref, Title: t.Title, Kind: t.Kind},
		dir: r.runDir(rec.Repo, rec.ID), agents: map[string]Agent{}, sessions: map[string]session{},
		res: &res, issue: t.Issue, maxIter: maxIter, perIssue: perIssue, budget: day,
		recovered: true,
	}
	res.RunDir = ru.dir
	r.openLog(ru)
	defer ru.closeLog()
	if rec.Kind != store.KindIssue {
		return res, r.haltUnresumable(ctx, ru)
	}
	if rec.Workspace != "" && r.Display != nil {
		ru.view = r.Display.Attach(rec.Workspace)
	}
	if rec.Worktree != "" && rec.Base != "" {
		ru.repo = prompts.Repo{Worktree: rec.Worktree, Branch: rec.Branch, Base: rec.Base}
	}
	// No agent to stop: omp sessions are children of the dead process and die with it.
	ru.log.Warn("recovering run", "run", rec.ID, "state", rec.State, "owner", who)
	if _, err := r.Store.FailRunningAgents(ctx, rec.ID); err != nil {
		ru.log.Warn("could not record agents", "err", err)
	}
	if _, err := r.Store.FailRunningSessions(ctx, rec.ID); err != nil {
		ru.log.Warn("could not record sessions", "err", err)
	}
	return res, r.finish(ctx, ru, t)
}

// haltUnresumable closes a review or address run whose process died: it has nothing to deliver,
// so only what it fetched is cleaned up before it is recorded as halted. It drops the same two
// things finishReview does — the worktree and the PR ref — because a ref left behind keeps the
// PR's commits reachable, and git never collects them: the repository would grow a copy of
// every branch delamain ever looked at, including the ones since deleted.
func (r *Runner) haltUnresumable(ctx context.Context, ru *run) error {
	rec := ru.rec
	ru.log.Warn("halting interrupted run", "run", rec.ID, "kind", rec.Kind, "state", rec.State)
	if rec.Worktree != "" {
		if _, err := os.Stat(rec.Worktree); err == nil {
			if err := r.Git.RemoveWorktree(ctx, rec.Repo, rec.Worktree); err != nil {
				ru.log.Warn("could not remove the worktree", "worktree", rec.Worktree, "err", err)
			}
		}
	}
	if n := prNumber(rec.IssueRef); n > 0 {
		if err := r.Git.DeleteRef(ctx, rec.Repo, gitx.PRRef(n)); err != nil {
			ru.log.Warn("could not remove the PR ref", "ref", gitx.PRRef(n), "err", err)
		}
	}
	if _, err := r.Store.FailRunningAgents(ctx, rec.ID); err != nil {
		ru.log.Warn("could not record agents", "err", err)
	}
	if _, err := r.Store.FailRunningSessions(ctx, rec.ID); err != nil {
		ru.log.Warn("could not record sessions", "err", err)
	}
	ru.res.Reason = "interrompida: run de review não é retomada"
	return r.record(ctx, ru)
}

func kindFromBranch(branch string) string {
	if strings.HasPrefix(branch, "fix/") {
		return "bug"
	}
	return "feature"
}

// ProcessAlive reports whether pid exists; EPERM means it exists under another user.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
