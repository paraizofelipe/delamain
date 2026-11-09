package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/control"
)

// errDirtyWorktree is control.ErrDirtyWorktree: a worktree with uncommitted changes, unless forced.
var errDirtyWorktree = control.ErrDirtyWorktree

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
			s := newStopper(cfg, st, true, 0, nil)
			defer s.close()
			failed := 0
			for _, arg := range args {
				id, err := runID(arg)
				if err == nil {
					err = s.removeOne(c.Context(), c.OutOrStdout(), id, force)
				}
				if err != nil {
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

// exists reports whether path is there; an error other than not-exist counts as there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
