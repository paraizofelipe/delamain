package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

func newRmCmd(load func() (config.Config, error)) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm RUN_ID...",
		Short: "Delete finished runs: their record, directory and leftover worktree",
		Long: `Deletes each finished run: its worktree, if one is left, its run directory (logs, sessions,
plan and report) and its record in the state database. A run that has not finished is refused
and nothing of it is deleted: stop it first with delamain stop. A worktree with uncommitted
changes, untracked files included, is refused too unless --force discards them, and so is a
worktree an unfinished run is working in, even with --force.

Each run is handled on its own: one that cannot be deleted does not keep the others.

Branches, local or remote, and PRs/MRs are never touched: the branch may be the draft the run
delivered. The run's token usage stays in the ledger, so deleting a run gives no daily budget
back.

Exit codes: 0 every run deleted · 1 a run could not be deleted.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			st, err := openStore(c.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			failed := 0
			for _, arg := range args {
				if err := removeRun(c, cfg, st, arg, force); err != nil {
					fmt.Fprintln(c.ErrOrStderr(), err)
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d runs failed", failed, len(args))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove a worktree even if it has uncommitted changes")
	return cmd
}

func removeRun(c *cobra.Command, cfg config.Config, st *store.Store, arg string, force bool) error {
	ctx := c.Context()
	id, err := runID(arg)
	if err != nil {
		return err
	}
	rec, err := st.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("run %d not found", id)
	}
	if err != nil {
		return err
	}
	if !rec.State.Terminal() {
		return fmt.Errorf("run %d is still %s: stop it first with 'delamain stop %d'", id, rec.State, id)
	}
	runs := filepath.Join(cfg.Paths.StateDir, "runs")
	dir := pipeline.RunDir(cfg.Paths.StateDir, rec.Repo, rec.ID)
	if !within(runs, dir) {
		return fmt.Errorf("run %d: its directory %s is outside %s", id, dir, runs)
	}
	if rec.Worktree != "" && exists(rec.Worktree) {
		if err := worktreeInUse(ctx, st, rec.Worktree); err != nil {
			return err
		}
		git := gitx.New()
		if !force {
			status, err := git.Status(ctx, rec.Worktree)
			if err != nil {
				return fmt.Errorf("run %d: %w", id, err)
			}
			if status != "" {
				return fmt.Errorf("worktree %s has uncommitted changes; use --force to discard them", rec.Worktree)
			}
		}
		if err := git.RemoveWorktree(ctx, rec.Repo, rec.Worktree); err != nil {
			return fmt.Errorf("run %d: %w", id, err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("run %d: %w", id, err)
	}
	if err := st.DeleteRun(ctx, id); err != nil {
		return fmt.Errorf("run %d: %w", id, err)
	}
	fmt.Fprintf(c.OutOrStdout(), "removed run %d\n", id)
	return nil
}

// worktreeInUse refuses a worktree an unfinished run works in: fixing a PR's review threads
// reuses the worktree of the issue run that opened it.
func worktreeInUse(ctx context.Context, st *store.Store, worktree string) error {
	active, err := st.UnfinishedRuns(ctx)
	if err != nil {
		return err
	}
	for _, a := range active {
		if samePath(worktree, a.Worktree) {
			return fmt.Errorf("worktree %s is in use by run %d (%s)", worktree, a.ID, a.State)
		}
	}
	return nil
}

// exists reports whether path is there; an error other than not-exist counts as there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
