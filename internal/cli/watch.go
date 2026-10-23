package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
)

// watchSource is everything `watch` reads; it has no method that changes a run.
type watchSource interface {
	// Runs returns the active runs, oldest first, and up to finished ended runs, newest first.
	Runs(ctx context.Context, finished int) (active, done []RunView, err error)
	Status(ctx context.Context, id int64) (RunStatus, error)
	Today(ctx context.Context) (budget.Status, error)
	DaemonRunning() bool
}

type watchOptions struct {
	limit    int           // finished runs to show
	interval time.Duration // between reloads
	now      func() time.Time
	actions  watchActions                                         // what s, d, o and y do
	after    func(time.Duration, func(time.Time) tea.Msg) tea.Cmd // the toast timer: tea.Tick
}

func newWatchCmd(load func() (config.Config, error)) *cobra.Command {
	opts := watchOptions{}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Follow runs, the daemon and the day's spend in a terminal UI",
		Long: `Open a terminal UI that lists the active runs and the latest finished ones, shows the status
of the selected run, whether the daemon is running and the day's spend, and reloads everything
every --interval. s stops the selected run as delamain stop --no-wait does and d removes it as
delamain rm does, each after a confirmation; o opens its PR/MR, y copies its ID and ? lists every
key. Quitting (q or ctrl+c) leaves every run as it is.

In tmux, open it in a floating popup:

  bind D display-popup -E -w 50% -h 60% "delamain watch"

y copies the run ID through OSC 52; tmux needs ` + "`set -g set-clipboard on`.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if opts.interval <= 0 {
				return errors.New("--interval must be greater than zero")
			}
			out, ok := c.OutOrStdout().(*os.File)
			if !ok || !term.IsTerminal(out.Fd()) {
				return errors.New("delamain watch needs a terminal; use 'delamain ls' or 'delamain status'")
			}
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			acts := newStoreWatchActions(ctx, in.cfg, in.st, out)
			defer acts.close()
			o := opts
			o.actions = acts
			return runWatch(ctx, c.InOrStdin(), out, inspectorWatchSource{in: in}, newStyles(lipgloss.NewRenderer(os.Stdout)), o)
		},
	}
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "finished runs to show")
	cmd.Flags().DurationVar(&opts.interval, "interval", time.Second, "time between reloads")
	return cmd
}

// runWatch runs the TUI on the alternate screen until the user quits or ctx ends.
func runWatch(ctx context.Context, in io.Reader, out io.Writer, src watchSource, st styles, opts watchOptions) error {
	p := tea.NewProgram(newWatchModel(src, st, opts), tea.WithAltScreen(), tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// inspectorWatchSource reads what `watch` shows from the store, as `ls` and `status` do.
type inspectorWatchSource struct {
	in *inspector
}

func (s inspectorWatchSource) Runs(ctx context.Context, finished int) (active, done []RunView, err error) {
	unfinished, err := s.in.st.UnfinishedRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The newest runs include the active ones: ask for enough to still get `finished` ended ones.
	recent, err := s.in.st.RecentRuns(ctx, max(finished, 0)+len(unfinished))
	if err != nil {
		return nil, nil, err
	}
	for _, rec := range unfinished {
		v, err := s.in.view(ctx, rec)
		if err != nil {
			return nil, nil, err
		}
		active = append(active, v)
	}
	for _, rec := range recent {
		if !rec.State.Terminal() || len(done) >= finished {
			continue
		}
		v, err := s.in.view(ctx, rec)
		if err != nil {
			return nil, nil, err
		}
		done = append(done, v)
	}
	return active, done, nil
}

// Status is `status` without the worktrees, which the panel does not show: it skips running git
// on every reload.
func (s inspectorWatchSource) Status(ctx context.Context, id int64) (RunStatus, error) {
	rec, err := s.in.st.GetRun(ctx, id)
	if err != nil {
		return RunStatus{}, fmt.Errorf("run %d: %w", id, err)
	}
	v, err := s.in.view(ctx, rec)
	if err != nil {
		return RunStatus{}, err
	}
	b, err := s.in.budgetView(ctx, rec)
	return RunStatus{Run: v, Budget: b}, err
}

func (s inspectorWatchSource) Today(ctx context.Context) (budget.Status, error) {
	return budget.Meter{Store: s.in.st, Cfg: s.in.cfg.Budget}.Today(ctx)
}

func (s inspectorWatchSource) DaemonRunning() bool { return daemonRunning(s.in.cfg.Paths.StateDir) }

// daemonRunning reports whether a daemon holds the lock of stateDir (see lockDaemon); it never
// creates the file, and a lock it gets is released at once.
func daemonRunning(stateDir string) bool {
	f, err := os.Open(filepath.Join(stateDir, "daemon.lock")) //nolint:gosec // inside the state dir
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fd fits in int
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // fd fits in int
	}
	return errors.Is(err, syscall.EWOULDBLOCK)
}
