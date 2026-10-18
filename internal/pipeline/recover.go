package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// Recover finalizes runs whose delamain process died (crash, reboot, kill): their agents are
// stopped, committed work is delivered as a draft, empty work is removed and the run is recorded
// as halted. It never resumes agent work: a half-finished turn is not trustworthy.
// forgeFor resolves each run's repository forge; nil keeps delivery local.
func (r *Runner) Recover(ctx context.Context, forgeFor func(repo string) (forge.Forge, error)) ([]Result, error) {
	runs, err := r.Store.UnfinishedRuns(ctx)
	if err != nil {
		return nil, err
	}
	alive := r.Alive
	if alive == nil {
		alive = processAlive
	}
	var out []Result
	for _, rec := range runs {
		if rec.OwnerPID == os.Getpid() || alive(rec.OwnerPID) {
			continue // still being driven by a live process
		}
		claimed, err := r.Store.ClaimRun(ctx, rec.ID, rec.OwnerPID, os.Getpid())
		if err != nil {
			return out, err
		}
		if !claimed {
			continue // another process is recovering it
		}
		rr := *r
		rr.Forge = nil
		if forgeFor != nil {
			fg, err := forgeFor(rec.Repo)
			if err != nil {
				r.Log.Warn("recover: no forge, delivering locally", "run", rec.ID, "err", err)
			} else {
				rr.Forge = fg
			}
		}
		res, err := rr.recoverRun(ctx, rec)
		if err != nil {
			return out, fmt.Errorf("recover run %d: %w", rec.ID, err)
		}
		out = append(out, res)
	}
	return out, nil
}

func (r *Runner) recoverRun(ctx context.Context, rec store.Run) (Result, error) {
	who := fmt.Sprintf("pid %d", rec.OwnerPID)
	if rec.OwnerPID == 0 {
		who = "pid desconhecido"
	}
	res := Result{
		RunID: rec.ID, Outcome: OutcomeHalted, Branch: rec.Branch, Base: rec.Base, Worktree: rec.Worktree,
		WorkspaceID: rec.Workspace, Iterations: rec.Iteration, PRURL: rec.MRURL,
		Reason: fmt.Sprintf("run interrompida: o processo do delamain (%s) parou no estado %s", who, rec.State),
	}
	t := Task{Ref: rec.IssueRef, Title: rec.Title, Kind: kindFromBranch(rec.Branch), Repo: rec.Repo, Issue: issueNumber(rec.IssueRef)}
	day, err := r.meter().Today(ctx)
	if err != nil {
		return res, err
	}
	ru := &run{
		rec: rec, task: prompts.Issue{Ref: t.Ref, Title: t.Title, Kind: t.Kind},
		dir: r.runDir(rec.Repo, rec.ID), agents: map[string]Agent{}, panes: map[string]string{}, tabs: map[string]string{},
		res: &res, issue: t.Issue, maxIter: r.Cfg.Pipeline.MaxIterations, budget: day,
		workspace: rec.Workspace, recovered: true,
	}
	res.RunDir = ru.dir
	if rec.Worktree != "" && rec.Base != "" {
		ru.repo = prompts.Repo{Worktree: rec.Worktree, Branch: rec.Branch, Base: rec.Base}
	}
	// No agent to stop: omp sessions are children of the dead process and die with it.
	r.Log.Warn("recovering run", "run", rec.ID, "state", rec.State, "owner", who)
	return res, r.finish(ctx, ru, t)
}

func kindFromBranch(branch string) string {
	if strings.HasPrefix(branch, "fix/") {
		return "bug"
	}
	return "feature"
}

// processAlive reports whether pid exists; EPERM means it exists under another user.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
