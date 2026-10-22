package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

// stopWaitPoll paces how often `stop` checks whether the run finished.
var stopWaitPoll = time.Second

func newStopCmd(load func() (config.Config, error)) *cobra.Command {
	var noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop RUN_ID...",
		Short: "Stop one run, leaving the other runs of its process alone",
		Long: `Asks the process driving each run to stop it. Nothing is killed: the request is recorded in
the state database, and the owner (delamain run, delamain review or the daemon) cancels only
that run. It ends as any interrupted run does: agents stopped, what was committed delivered as a
draft, the issue labeled for a human, report.md written and the halted toast sent.

A run that already finished is left as it is. A run whose owner died is finalized right away,
the way delamain recover does, but only that one.

Without --no-wait, stop waits until the run finishes and prints its state and result. A run
that does not stop within --timeout is an error; its request stays recorded and the owner stops
it as soon as it notices.

Exit codes: 0 every run stopped or already finished · 1 a run failed or timed out.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			ctx := c.Context()
			st, err := openStore(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			failed := 0
			for _, arg := range args {
				if err := stopRun(c, st, arg, noWait, timeout); err != nil {
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
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "record the request and return without waiting for the run")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to wait for the run to stop")
	return cmd
}

func stopRun(c *cobra.Command, st *store.Store, arg string, noWait bool, timeout time.Duration) error {
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
	w := c.OutOrStdout()
	if rec.State.Terminal() {
		fmt.Fprintf(w, "run %d already finished (%s)\n", id, rec.State)
		return nil
	}
	if err := st.RequestStop(ctx, id); err != nil {
		return fmt.Errorf("run %d: %w", id, err)
	}
	if noWait {
		fmt.Fprintf(w, "stop requested for run %d (owner pid %d)\n", id, rec.OwnerPID)
		return nil
	}
	rec, err = waitStopped(ctx, st, id, timeout)
	if err != nil {
		return err
	}
	return printStopped(w, rec)
}

// waitStopped polls the run until it is terminal, checking at least once.
func waitStopped(ctx context.Context, st *store.Store, id int64, timeout time.Duration) (store.Run, error) {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := st.GetRun(ctx, id)
		if err != nil {
			return rec, fmt.Errorf("run %d: %w", id, err)
		}
		if rec.State.Terminal() {
			return rec, nil
		}
		if !time.Now().Before(deadline) {
			return rec, fmt.Errorf("run %d did not stop within %s; the request stays recorded", id, timeout)
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(min(stopWaitPoll, time.Until(deadline))):
		}
	}
}

// printStopped shows how the run ended with the STATE and RESULT lines of `status`.
func printStopped(w io.Writer, rec store.Run) error {
	st := newStyles(lipgloss.NewRenderer(w))
	return writeTable(w, [][]cell{
		{styled(st.label, "RUN"), plain(fmt.Sprintf("%d · %s %s", rec.ID, rec.IssueRef, rec.Title))},
		{styled(st.label, "STATE"), styled(st.runStateStyle(string(rec.State)), string(rec.State))},
		{styled(st.label, "RESULT"), resultCell(st, rec.Reason)},
	})
}
