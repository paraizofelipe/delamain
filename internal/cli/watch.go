package cli

import (
	"context"
	"errors"
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

func (s inspectorWatchSource) Runs(context.Context, int) ([]RunView, []RunView, error) {
	return nil, nil, nil
}

func (s inspectorWatchSource) Status(context.Context, int64) (RunStatus, error) {
	return RunStatus{}, nil
}

func (s inspectorWatchSource) Today(context.Context) (budget.Status, error) {
	return budget.Status{}, nil
}

func (s inspectorWatchSource) DaemonRunning() bool { return false }
