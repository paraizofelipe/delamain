package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
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
			s := &stopper{cfg: cfg, st: st, noWait: noWait, timeout: timeout}
			defer s.close()
			failed := 0
			for _, arg := range args {
				if err := s.stop(c, arg); err != nil {
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

// stopper stops runs one by one; the runner that recovers orphans is only built for one.
type stopper struct {
	cfg     config.Config
	st      *store.Store
	noWait  bool
	timeout time.Duration
	runner  *pipeline.Runner
	// log is where the recover of an orphan writes; nil leaves it on stderr, as the commands
	// want it. `watch` holds the terminal and passes a logger that drops the lines.
	log *slog.Logger
}

func (s *stopper) close() {
	if s.runner != nil && s.runner.Notify != nil {
		_ = s.runner.Notify.Close()
	}
}

func (s *stopper) stop(c *cobra.Command, arg string) error {
	id, err := runID(arg)
	if err != nil {
		return err
	}
	return s.stopOne(c.Context(), c.OutOrStdout(), id)
}

// stopOne stops run id and writes to w how it went.
func (s *stopper) stopOne(ctx context.Context, w io.Writer, id int64) error {
	rec, err := s.st.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("run %d not found", id)
	}
	if err != nil {
		return err
	}
	if rec.State.Terminal() {
		fmt.Fprintf(w, "run %d already finished (%s)\n", id, rec.State)
		return nil
	}
	if rec.OwnerPID != os.Getpid() && !pipeline.ProcessAlive(rec.OwnerPID) {
		return s.recoverOne(ctx, w, id)
	}
	if err := s.st.RequestStop(ctx, id); err != nil {
		return fmt.Errorf("run %d: %w", id, err)
	}
	if s.noWait {
		fmt.Fprintf(w, "stop requested for run %d (owner pid %d)\n", id, rec.OwnerPID)
		return nil
	}
	rec, err = waitStopped(ctx, s.st, id, s.timeout)
	if err != nil {
		return err
	}
	return printStopped(w, rec)
}

// recoverOne finalizes a run whose owner died, as `delamain recover` does, but only that one.
func (s *stopper) recoverOne(ctx context.Context, w io.Writer, id int64) error {
	if s.runner == nil {
		r, err := newRunner(s.cfg, s.st, notify.Options{Log: s.log})
		if err != nil {
			return err
		}
		s.runner = r
	}
	res, err := s.runner.RecoverRun(ctx, id, func(repo string) (forge.Forge, error) { return forgeFor(ctx, s.cfg, repo) })
	if err != nil {
		return err
	}
	reportRecovered(ctx, w, s.runner, []pipeline.Result{res})
	return nil
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
