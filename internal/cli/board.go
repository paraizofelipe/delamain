package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

type boardOptions struct {
	addr        string
	interval    time.Duration
	limit       int
	open        bool
	allowRemote bool
}

func newBoardCmd(load func() (config.Config, error)) *cobra.Command {
	opts := boardOptions{}
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Serve a live kanban of the runs in the browser",
		Long: `Serve a read-only kanban of the runs on a local address: the queue, one column per stage of
the pipeline (setup, plan, implement, verify, review, deliver), the finished runs and the ones that
need a person. Each card shows the verdict of the gates and of every review axis per iteration, and
opens the decisions of the agents: the plan, what the implementer did, the gates and the findings.

The page updates itself through Server-Sent Events as the runs move; the board itself never changes
a run. It listens on loopback only unless --allow-remote is given, since it shows titles, findings
and logs.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := opts.validate(); err != nil {
				return err
			}
			ctx := c.Context()
			in, done, err := openInspector(ctx, load)
			if err != nil {
				return err
			}
			defer done()
			ln, err := net.Listen("tcp", opts.addr)
			if err != nil {
				return err
			}
			url := "http://" + ln.Addr().String() + "/"
			fmt.Fprintf(c.OutOrStdout(), "board at %s (ctrl+c stops it)\n", url)
			if opts.open {
				if err := openBrowser(url); err != nil {
					fmt.Fprintln(c.ErrOrStderr(), err)
				}
			}
			return serveBoard(ctx, ln, boardSource{in: in}, opts)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", "127.0.0.1:7777", "address to listen on")
	cmd.Flags().DurationVar(&opts.interval, "interval", time.Second, "time between reloads")
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "finished runs to show")
	cmd.Flags().BoolVar(&opts.open, "open", false, "open the board in the browser")
	cmd.Flags().BoolVar(&opts.allowRemote, "allow-remote", false, "listen on an address other than loopback")
	return cmd
}

func (o boardOptions) validate() error {
	if o.interval <= 0 {
		return errors.New("--interval must be greater than zero")
	}
	if o.limit < 0 {
		return errors.New("--limit must not be negative")
	}
	host, _, err := net.SplitHostPort(o.addr)
	if err != nil {
		return fmt.Errorf("--addr: %w", err)
	}
	if !o.allowRemote && !loopback(host) {
		return fmt.Errorf("--addr %s is not a loopback address: the board shows titles, findings and logs; pass --allow-remote to listen there anyway", o.addr)
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveBoard serves the board on ln until ctx ends.
func serveBoard(ctx context.Context, ln net.Listener, src board.Source, opts boardOptions) error {
	srv := board.New(src, board.Options{Interval: opts.interval, Limit: opts.limit, LocalOnly: !opts.allowRemote})
	go srv.Run(ctx)
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// The event streams end with ctx, so a shutdown does not wait on open pages.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// browserOpener is the command that hands a URL to the desktop's browser.
func browserOpener() string {
	if runtime.GOOS == "darwin" {
		return "open"
	}
	return "xdg-open"
}

// openBrowser opens the board's own loopback URL, without waiting for the browser.
func openBrowser(url string) error {
	cmd := exec.Command(browserOpener(), url) //nolint:gosec // the board's URL as the only argument, no shell
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open the browser: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// boardSource reads what the board shows from the store, as `watch` does.
type boardSource struct {
	in *inspector
}

func (s boardSource) Runs(ctx context.Context, finished int) ([]board.Run, error) {
	active, done, err := inspectorWatchSource(s).Runs(ctx, finished)
	if err != nil {
		return nil, err
	}
	runs := make([]board.Run, 0, len(active)+len(done))
	for _, v := range append(active, done...) {
		runs = append(runs, s.run(v))
	}
	return runs, nil
}

func (s boardSource) Run(ctx context.Context, id int64) (board.Run, error) {
	rec, err := s.in.st.GetRun(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return board.Run{}, fmt.Errorf("run %d: %w", id, board.ErrNotFound)
	}
	if err != nil {
		return board.Run{}, err
	}
	v, err := s.in.view(ctx, rec)
	if err != nil {
		return board.Run{}, err
	}
	return s.run(v), nil
}

func (s boardSource) run(v RunView) board.Run {
	r := board.Run{
		ID: v.ID, Kind: v.Kind, Repo: v.Repo, Issue: v.Issue, Title: v.Title, State: v.State, Reason: v.Result,
		Iteration: v.Iteration, MaxIterations: v.iterations, Complexity: v.Complexity, Branch: v.Branch, PR: v.PR,
		// An active run whose owner died waits for `delamain recover`.
		Orphaned: !v.terminal && v.Owner.PID != 0 && !v.Owner.Alive,
		Dir:      v.RunDir, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Agents: []board.Agent{},
	}
	var usage budget.Status
	usage.Mode = s.in.cfg.Budget.Mode
	for _, a := range v.Agents {
		r.Agents = append(r.Agents, board.Agent{
			Role: a.Role, State: a.State, StartedAt: a.StartedAt, EndedAt: a.EndedAt, Tokens: a.Tokens, CostUSD: a.CostUSD,
		})
		usage.Usage.CostUSD += a.CostUSD
		usage.Run += float64(a.Tokens)
	}
	if usage.Mode == config.BudgetUSD {
		usage.Run = usage.Usage.CostUSD
	}
	if usage.Run > 0 || usage.Usage.CostUSD > 0 {
		r.Spend = usage.Summary()
	}
	return r
}

func (s boardSource) Queue(ctx context.Context) ([]board.QueueItem, error) {
	items, err := s.in.st.Queue(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]board.QueueItem, 0, len(items))
	for _, it := range items {
		out = append(out, board.QueueItem{Position: it.Position, Repo: it.Repo, Issue: it.Issue, Title: it.Title, AddedAt: it.AddedAt})
	}
	return out, nil
}

func (s boardSource) Status(ctx context.Context) (board.Status, error) {
	src := inspectorWatchSource(s)
	slots, err := src.Slots(ctx)
	if err != nil {
		return board.Status{}, err
	}
	today, err := src.Today(ctx)
	if err != nil {
		return board.Status{}, err
	}
	st := board.Status{
		Daemon: src.DaemonRunning(), SlotsInUse: slots.InUse, SlotsMax: slots.Max, QueueRunning: slots.Queue,
		Today: budget.Format(today.Day, today.Mode), TodayLevel: today.Level.String(),
	}
	if today.Daily > 0 {
		st.TodayLimit = budget.Format(today.Daily, today.Mode)
	}
	return st, nil
}
