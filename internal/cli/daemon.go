package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/daemon"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

func newDaemonCmd(load func() (config.Config, error)) *cobra.Command {
	var repos []string
	cmd := &cobra.Command{
		Use:   "daemon [--repo <path>]...",
		Short: "Continuously process the issues, and the PRs/MRs up for review, of the configured repositories",
		Long: `Polls open issues with forge.labels.ready every daemon.poll_interval and runs up to
pipeline.max_parallel at once. With review.enabled, it also reviews the open, non-draft PRs/MRs
labeled forge.labels.review, once per head, in the same slots and daily budget; a head whose
review halts is retried a few times and then left to the next push. On startup, finalizes runs
left behind by a process that died. Only one instance runs per state_dir. Ctrl+C stops it:
in-flight runs end as interrupted.`,
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
			var hosts []string
			users := map[string]string{} // repo → forge login a review posts as
			for _, repo := range repos {
				fg, err := forgeFor(ctx, cfg, repo)
				if err != nil {
					return fmt.Errorf("%s: %w", repo, err)
				}
				if fg == nil {
					return fmt.Errorf("%s: the daemon requires a GitHub or GitLab origin", repo)
				}
				sources = append(sources, daemon.Source{Repo: repo, Forge: fg})
				if cfg.Review.Enabled {
					// Only comments by this login count as delamain's own when deduplicating.
					id, err := forgeAuth(ctx, cfg, repo)
					if err != nil {
						return fmt.Errorf("%s: could not identify the forge user that posts the review: %w", repo, err)
					}
					users[repo] = id.Login
				}
				if h := fg.Repo().Host; !slices.Contains(hosts, h) {
					hosts = append(hosts, h)
				}
			}
			st, err := openStore(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			// Only the daemon is interactive: its toasts open the PR, issue or report on click.
			base, err := newRunner(cfg, st, notify.Options{Interactive: true, ForgeHosts: hosts})
			if err != nil {
				return err
			}
			defer func() { _ = base.Notify.Close() }()
			if err := recoverRuns(c, cfg, base); err != nil {
				return err
			}
			meter := budget.Meter{Store: st, Cfg: cfg.Budget}
			issues := daemon.IssueJobs{Label: cfg.Forge.Labels.Ready, Run: func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error) {
				r := *base // per-run copy: each repository has its own forge
				r.Forge = fg
				return r.Run(ctx, t)
			}}
			reviews := daemon.ReviewJobs{
				Enabled: cfg.Review.Enabled, Label: cfg.Forge.Labels.Review,
				// The review records its cursor under the repository's toplevel, not the path given.
				State: func(ctx context.Context, repo string, pr int) (daemon.ReviewState, error) {
					top, err := base.Git.Toplevel(ctx, repo)
					if err != nil {
						return daemon.ReviewState{}, err
					}
					cur, err := st.Cursor(ctx, top, pr)
					if errors.Is(err, store.ErrNotFound) {
						return daemon.ReviewState{}, nil
					}
					if err != nil {
						return daemon.ReviewState{}, err
					}
					return daemon.ReviewState{ReviewedHead: cur.LastHeadSHA, HaltedHead: cur.HaltedHeadSHA, Halts: cur.Halts}, nil
				},
				Run: func(ctx context.Context, repo string, pr int, fg forge.Forge) (pipeline.Result, error) {
					r := *base
					r.Forge, r.ForgeUser = fg, users[repo]
					return r.RunReview(ctx, repo, pr)
				},
			}
			d := daemon.New(daemon.Options{
				Sources: sources, Producers: daemon.Producers(base.Log, issues, reviews),
				MaxParallel: cfg.Pipeline.MaxParallel, Interval: cfg.Daemon.PollInterval, Log: base.Log,
				DayExhausted: dayExhausted(meter.Today, &budgetAlert{notify: base.Notify, softRatio: cfg.Budget.SoftRatio, now: time.Now}),
			})
			base.Log.Info("daemon started", "repos", len(sources), "max_parallel", cfg.Pipeline.MaxParallel, "poll", cfg.Daemon.PollInterval)
			base.Log.Info(reviewStartup(cfg))
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
			base, err := newRunner(cfg, st, notify.Options{})
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
	var recovered []notify.Recovered
	for _, res := range results {
		fmt.Fprintf(c.OutOrStdout(), "run %d recovered:\n", res.RunID)
		printResult(c, res)
		rec, err := base.Store.GetRun(ctx, res.RunID)
		if err != nil { // the toast is a courtesy: it never fails the recovery
			base.Log.Warn("recover toast: run not found", "run", res.RunID, "err", err)
			continue
		}
		recovered = append(recovered, notify.Recovered{Repo: rec.Repo, Ref: rec.IssueRef, Result: recoveredResult(res)})
	}
	if base.Notify != nil && len(recovered) > 0 {
		_ = base.Notify.Notify(ctx, notify.FormatRecover(recovered))
	}
	return nil
}

// recoveredResult says how a recovered run ended, in the recover toast's words.
func recoveredResult(res pipeline.Result) string {
	switch {
	case res.PRURL != "" && !res.Draft:
		return "PR opened"
	case res.PRURL != "":
		return "draft PR"
	case res.Outcome == pipeline.OutcomeNeedsInfo:
		return "needs info"
	case res.Outcome == pipeline.OutcomeHalted:
		return "halted"
	}
	return "needs review" // exhausted, or approved without a PR, as the needs_human toast
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

// dayExhausted evaluates the daily budget on each poll: it alerts on the soft and hard levels and
// returns the reason to start no run once the hard one is reached.
func dayExhausted(today func(context.Context) (budget.Status, error), alert *budgetAlert) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		s, err := today(ctx)
		if err != nil {
			return "", err
		}
		alert.observe(ctx, s)
		if s.Level != budget.Hard {
			return "", nil
		}
		return s.Reason, nil
	}
}

// budgetAlert notifies each daily budget level at most once a day; it forgets at midnight.
type budgetAlert struct {
	notify    notify.Notifier
	softRatio float64
	now       func() time.Time
	day       string
	sent      map[budget.Level]bool
}

func (a *budgetAlert) observe(ctx context.Context, s budget.Status) {
	if a.notify == nil || s.Level == budget.OK {
		return
	}
	if day := a.now().Format(time.DateOnly); day != a.day {
		a.day, a.sent = day, map[budget.Level]bool{}
	}
	if a.sent[s.Level] {
		return
	}
	a.sent[s.Level] = true
	_ = a.notify.Notify(ctx, notify.FormatBudget(notify.Budget{
		Hard: s.Level == budget.Hard, Percent: int(math.Round(a.softRatio * 100)),
		Used: budget.Format(s.Day, s.Mode), Limit: budget.Format(s.Daily, s.Mode),
		CostUSD: s.DayUsage.CostUSD, USD: s.Mode == config.BudgetUSD,
	}))
}

// reviewStartup says whether the daemon reviews PRs/MRs, for the startup log. Review runs on
// either harness; a PR/MR whose head is in a fork is never reviewed, on any of them.
func reviewStartup(cfg config.Config) string {
	if !cfg.Review.Enabled {
		return "review: disabled"
	}
	return fmt.Sprintf("review: enabled (label %s; PRs/MRs from a fork are not reviewed)", cfg.Forge.Labels.Review)
}
