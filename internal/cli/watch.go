package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
}

func newWatchCmd(load func() (config.Config, error)) *cobra.Command {
	opts := watchOptions{}
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Follow runs, the daemon and the day's spend in a terminal UI",
		Long: `Open a read-only terminal UI that lists the active runs and the latest finished ones, shows
the status of the selected run, whether the daemon is running and the day's spend, and reloads
everything every --interval. Quitting (q or ctrl+c) leaves every run as it is.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
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
			return runWatch(ctx, c.InOrStdin(), out, inspectorWatchSource{in: in}, newStyles(lipgloss.NewRenderer(os.Stdout)), opts)
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

func (s inspectorWatchSource) DaemonRunning() bool { return false }
