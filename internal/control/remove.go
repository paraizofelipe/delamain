package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/paraizo/delamain/internal/pipeline"
)

// Remove deletes finished run id: its worktree, if one is left, its run directory and its
// record. A run that has not finished is refused, and so is a worktree with uncommitted changes
// (unless force discards them) or one an unfinished run is working in (even with force).
// Branches and PRs are never touched; the token ledger keeps the run's spend.
func (a *Actions) Remove(ctx context.Context, id int64, force bool) error {
	rec, err := a.getRun(ctx, id)
	if err != nil {
		return err
	}
	if !rec.State.Terminal() {
		return &RunError{ID: id, State: rec.State, Err: ErrRunNotFinished}
	}
	runs := filepath.Join(a.d.Cfg.Paths.StateDir, "runs")
	dir := pipeline.RunDir(a.d.Cfg.Paths.StateDir, rec.Repo, rec.ID)
	if !within(runs, dir) {
		return &RunError{ID: id, Err: fmt.Errorf("%w: %s is outside %s", ErrOutsideState, dir, runs)}
	}
	if rec.Worktree != "" && exists(rec.Worktree) {
		if err := a.worktreeInUse(ctx, rec.Worktree); err != nil {
			return err
		}
		if !force {
			status, err := a.d.Git.Status(ctx, rec.Worktree)
			if err != nil {
				return &RunError{ID: id, Err: err}
			}
			if status != "" {
				return &RunError{ID: id, Worktree: rec.Worktree, Err: ErrDirtyWorktree}
			}
		}
		if err := a.d.Git.RemoveWorktree(ctx, rec.Repo, rec.Worktree); err != nil {
			return &RunError{ID: id, Err: err}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return &RunError{ID: id, Err: err}
	}
	if err := a.d.Store.DeleteRun(ctx, id); err != nil {
		return &RunError{ID: id, Err: err}
	}
	return nil
}

// worktreeInUse refuses a worktree an unfinished run works in: fixing a PR's review threads
// reuses the worktree of the issue run that opened it.
func (a *Actions) worktreeInUse(ctx context.Context, worktree string) error {
	active, err := a.d.Store.UnfinishedRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range active {
		if samePath(worktree, r.Worktree) {
			return &RunError{ID: r.ID, State: r.State, Worktree: worktree, UsedBy: r.ID, Err: ErrWorktreeInUse}
		}
	}
	return nil
}
