package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/daemon"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
)

func newDaemonCmd(load func() (config.Config, error)) *cobra.Command {
	var repos []string
	cmd := &cobra.Command{
		Use:   "daemon [--repo <path>]...",
		Short: "Continuously process issues labeled ready in the configured repositories",
		Long: `Polls open issues with forge.labels.ready every daemon.poll_interval and runs up to
pipeline.max_parallel at once. On startup, finalizes runs left behind by a process that died.
Only one instance runs per state_dir. Ctrl+C stops it: in-flight runs end as interrupted.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			applyHerdrFlag(c, &cfg)
			if len(repos) == 0 {
				repos = cfg.Daemon.Repos
			}
			if len(repos) == 0 {
				return errors.New("no repository: use --repo or daemon.repos in the config")
			}
			ctx := c.Context()
			unlock, err := lockDaemon(cfg.Paths.StateDir)
			if err != nil {
				return err
			}
			defer unlock()

			var sources []daemon.Source
			for _, repo := range repos {
				fg, err := forgeFor(ctx, cfg, repo)
				if err != nil {
					return fmt.Errorf("%s: %w", repo, err)
				}
				if fg == nil {
					return fmt.Errorf("%s: the daemon requires a GitHub or GitLab origin", repo)
				}
				sources = append(sources, daemon.Source{Repo: repo, Forge: fg})
			}
			st, err := openStore(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			base, err := newRunner(cfg, st, true)
			if err != nil {
				return err
			}
			defer func() { _ = base.Notify.Close() }()
			if err := recoverRuns(c, cfg, base); err != nil {
				return err
			}
			meter := budget.Meter{Store: st, Cfg: cfg.Budget}
			alerts := newBudgetAlerts(cfg, base.Notify)
			d := daemon.New(daemon.Options{
				Sources: sources, Label: cfg.Forge.Labels.Ready, MaxParallel: cfg.Pipeline.MaxParallel,
				Interval: cfg.Daemon.PollInterval, Log: base.Log,
				Run: func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error) {
					r := *base // per-run copy: each repository has its own forge
					r.Forge = fg
					return r.Run(ctx, t)
				},
				DayExhausted: func(ctx context.Context) (string, error) {
					s, err := meter.Today(ctx)
					if err != nil {
						return "", err
					}
					alerts.check(ctx, s, time.Now())
					if s.Level != budget.Hard {
						return "", nil
					}
					return s.Reason, nil
				},
			})
			base.Log.Info("daemon started", "repos", len(sources), "max_parallel", cfg.Pipeline.MaxParallel, "poll", cfg.Daemon.PollInterval)
			return d.Serve(ctx)
		},
	}
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "repository to watch (repeatable; default: daemon.repos)")
	addHerdrFlag(cmd)
	return cmd
}

func newRecoverCmd(load func() (config.Config, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "recover",
		Short: "Finalize runs left behind by a delamain process that died",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			st, err := openStore(c.Context(), cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			base, err := newRunner(cfg, st, false)
			if err != nil {
				return err
			}
			defer func() { _ = base.Notify.Close() }()
			return recoverRuns(c, cfg, base)
		},
	}
}

func recoverRuns(c *cobra.Command, cfg config.Config, base *pipeline.Runner) error {
	ctx := c.Context()
	results, err := base.Recover(ctx, func(repo string) (forge.Forge, error) { return forgeFor(ctx, cfg, repo) })
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(c.OutOrStdout(), "no orphaned run to recover")
		return nil
	}
	for _, res := range results {
		fmt.Fprintf(c.OutOrStdout(), "run %d recovered:\n", res.RunID)
		printResult(c, res)
	}
	return nil
}

// lockDaemon allows one daemon per state dir: two schedulers would race for the same issues.
func lockDaemon(stateDir string) (func(), error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // inside the state dir
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		f.Close()
		return nil, fmt.Errorf("another delamain daemon is already using %s", stateDir)
	}
	slog.Debug("daemon lock acquired", "path", path)
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil //nolint:gosec // fd fits in int
}

// budgetAlerts tells the desktop when the daily budget reaches its soft or hard limit, at most
// once per level per day.
type budgetAlerts struct {
	cfg    config.Config
	notify notify.Notifier
	day    string
	sent   map[budget.Level]bool
}

func newBudgetAlerts(cfg config.Config, n notify.Notifier) *budgetAlerts {
	return &budgetAlerts{cfg: cfg, notify: n}
}

// check looks at the day's status s, evaluated at now.
func (a *budgetAlerts) check(ctx context.Context, s budget.Status, now time.Time) {
	if day := now.Format(time.DateOnly); day != a.day {
		a.day, a.sent = day, map[budget.Level]bool{}
	}
	if s.Level == budget.OK || a.sent[s.Level] || !notify.Allowed(a.cfg.Notify, notify.EventBudget) {
		return
	}
	a.sent[s.Level] = true
	_ = a.notify.Notify(ctx, notify.FormatBudget(notify.Budget{
		Hard: s.Level == budget.Hard, Pct: int(math.Round(a.cfg.Budget.SoftRatio * 100)),
		Used: budget.Format(s.Day, s.Mode), Limit: budget.Format(s.Daily, s.Mode),
		CostUSD: s.DayCostUSD, USD: s.Mode == config.BudgetUSD,
	}))
}
