package cli

import (
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
	cmd := &cobra.Command{
		Use:   "rm RUN_ID...",
		Short: "Delete finished runs: their record, directory and leftover worktree",
		Long: `Deletes each finished run: its worktree, if one is left, its run directory (logs, sessions,
plan and report) and its record in the state database. A run that has not finished is refused
and nothing of it is deleted: stop it first with delamain stop.

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
			for _, arg := range args {
				if err := removeRun(c, cfg, st, arg); err != nil {
					fmt.Fprintln(c.ErrOrStderr(), err)
					return fmt.Errorf("1 of %d runs failed", len(args))
				}
			}
			return nil
		},
	}
	return cmd
}

func removeRun(c *cobra.Command, cfg config.Config, st *store.Store, arg string) error {
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
		if err := gitx.New().RemoveWorktree(ctx, rec.Repo, rec.Worktree); err != nil {
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

// exists reports whether path is there; an error other than not-exist counts as there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
