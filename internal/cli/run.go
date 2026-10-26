package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// ExitError carries a process exit code for outcomes that are not crashes.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Exit codes of `delamain run` and `delamain review`, so scripts can branch on the outcome.
var outcomeExit = map[pipeline.Outcome]int{
	pipeline.OutcomeApproved:  0,
	pipeline.OutcomeHalted:    1,
	pipeline.OutcomeExhausted: 3,
	pipeline.OutcomeNeedsInfo: 4,
	pipeline.OutcomeReviewed:  0,
	pipeline.OutcomeAnswered:  0,
	pipeline.OutcomeSkipped:   0,
}

func newRunCmd(load func() (config.Config, error)) *cobra.Command {
	var taskFile, repo, kind string
	var issue int
	cmd := &cobra.Command{
		Use:   "run (--task <file.md> | --issue <n>) [--repo <path>]",
		Short: "Run a task or issue: plan, TDD implementation, verification, review and PR/MR",
		Long: `Runs a markdown task ("# Title" + a description with acceptance criteria) or a GitHub/GitLab
issue in a git repository. Creates an isolated worktree in paths.worktrees_dir. Each agent
(planner, implementer, reviewer) runs as omp in RPC mode and only starts at its stage; the gates
run inside delamain. Follow it with "delamain logs <run> -f" and "delamain status <run>". With
--herdr (or herdr.enabled), the run also gets a herdr workspace with a "checks" tab and one tab
per agent. At the end it pushes the branch and opens the PR/MR (draft when not approved). It never merges.

pipeline.max_parallel bounds the runs of every delamain process together. While the queue
(delamain queue) has issues, one slot is reserved for it, and runs share the others. With no slot
free for it, the run does not start: it says the maximum parallelism was reached and exits with
code 5. delamain review does not take a slot.

Exit codes: 0 approved · 1 interrupted · 3 iterations exhausted · 4 task lacks enough information ·
5 maximum parallelism reached.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			applyHerdrFlag(c, &cfg)
			if (taskFile == "") == (issue == 0) {
				return errors.New("provide exactly one of --task or --issue")
			}
			if repo == "" {
				if repo, err = os.Getwd(); err != nil {
					return err
				}
			}
			fg, err := forgeFor(c.Context(), cfg, repo)
			if err != nil {
				return err
			}
			var task pipeline.Task
			if issue > 0 {
				if task, err = issueTask(c.Context(), fg, issue, repo); err != nil {
					return err
				}
			} else if task, err = fileTask(taskFile, repo); err != nil {
				return err
			}
			if kind != "" {
				task.Kind = kind
			}

			res, err := runTask(c.Context(), cfg, task, fg)
			if errors.Is(err, errNoSlot) {
				fmt.Fprintln(c.ErrOrStderr(), err)
				return ExitError{Code: exitNoSlot}
			}
			if err != nil {
				return err
			}
			printResult(c.OutOrStdout(), res)
			if code := outcomeExit[res.Outcome]; code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&taskFile, "task", "", "markdown task file")
	cmd.Flags().IntVar(&issue, "issue", 0, "issue number in the origin forge")
	cmd.Flags().StringVar(&repo, "repo", "", "target repository (default: current directory)")
	cmd.Flags().StringVar(&kind, "kind", "", "feature|bug (default: inferred from the title)")
	addHerdrFlag(cmd)
	return cmd
}

// addHerdrFlag lets one execution show its runs in herdr, overriding herdr.enabled.
func addHerdrFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("herdr", false, "show runs in herdr workspaces (default: herdr.enabled)")
}

func applyHerdrFlag(c *cobra.Command, cfg *config.Config) {
	if c.Flags().Changed("herdr") {
		cfg.Herdr.Enabled, _ = c.Flags().GetBool("herdr")
	}
}

func fileTask(path, repo string) (pipeline.Task, error) {
	title, body, err := pipeline.ParseTaskFile(path)
	if err != nil {
		return pipeline.Task{}, err
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	return pipeline.Task{Ref: "task:" + gitx.Slug(stem, 24), Title: title, Body: body, Kind: pipeline.InferKind(title, nil), Repo: repo}, nil
}

func issueTask(ctx context.Context, fg forge.Forge, n int, repo string) (pipeline.Task, error) {
	if fg == nil {
		return pipeline.Task{}, errors.New("--issue requires a repository with a GitHub or GitLab origin")
	}
	is, err := fg.Issue(ctx, n)
	if err != nil {
		return pipeline.Task{}, err
	}
	if !is.Open {
		return pipeline.Task{}, fmt.Errorf("issue #%d is closed", n)
	}
	return pipeline.TaskFromIssue(is, repo), nil
}

// runTask takes a free slot and runs task in it; with none free it returns errNoSlot.
func runTask(ctx context.Context, cfg config.Config, task pipeline.Task, fg forge.Forge) (pipeline.Result, error) {
	cfg, _, err := projectConfig(cfg, task.Repo)
	if err != nil {
		return pipeline.Result{}, err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = st.Close() }()
	release, err := acquireRunSlot(ctx, st, cfg.Pipeline.MaxParallel)
	if err != nil {
		return pipeline.Result{}, err
	}
	defer release()
	r, err := newRunner(cfg, st, notify.Options{})
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = r.Notify.Close() }()
	r.Forge = fg
	leaveQueueOnCreate(r, st, task, slog.Warn)
	return r.Run(ctx, task)
}

// newNotifier creates the desktop notifier; tests replace it.
var newNotifier = notify.New

// newRunner wires the configured agent harness, git, prompts, the desktop notifier (no says whether
// it is interactive) and, when enabled, herdr; the caller sets Forge per repository and closes
// the notifier.
func newRunner(cfg config.Config, st *store.Store, no notify.Options) (*pipeline.Runner, error) {
	set, err := loadPrompts(cfg)
	if err != nil {
		return nil, err
	}
	var display pipeline.Display
	if cfg.Herdr.Enabled {
		display = pipeline.HerdrDisplay{
			Herdr:      herdr.New(herdr.ExecRunner{Binary: cfg.Herdr.Binary, Session: cfg.Herdr.Session}),
			QuietShell: filepath.Join(cfg.Paths.StateDir, "herdr-shell"),
		}
	}
	target := promptsTarget(cfg)
	log := no.Log // a caller that holds the terminal, as `watch` does, passes its own
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	no.StateDir, no.Log = cfg.Paths.StateDir, log
	return &pipeline.Runner{
		Cfg:           cfg,
		Display:       display,
		Agents:        agentsFor(cfg, target),
		Checks:        pipeline.ExecChecks{},
		Git:           gitx.New(),
		Store:         st,
		Prompts:       set,
		PromptsTarget: target,
		Log:           log,
		Notify:        newNotifier(cfg.Notify, no),
	}, nil
}

// projectConfig merges the repository's .delamain.yaml agent overrides (agent, claude, omp,
// agents) into cfg, so each project picks how and which code agent runs it; errors name the
// file and surface before any agent starts.
func projectConfig(cfg config.Config, repo string) (config.Config, bool, error) {
	rc, err := gates.LoadRepoConfig(repo)
	if err != nil {
		return cfg, false, err
	}
	merged, changed, err := cfg.WithProjectOverrides(rc.Agent, rc.Claude, rc.Omp, rc.Agents)
	if err != nil {
		return cfg, changed, fmt.Errorf("%s: %w", gates.RepoConfigFile, err)
	}
	return merged, changed, nil
}

// retarget points a runner copy at the merged config's harness.
func retarget(r *pipeline.Runner, merged config.Config) {
	r.Cfg = merged
	r.PromptsTarget = promptsTarget(merged)
	r.Agents = agentsFor(merged, r.PromptsTarget)
}

// agentsFor starts the agents on the configured harness.
func agentsFor(cfg config.Config, target prompts.Target) pipeline.Agents {
	if cfg.Agent.Harness == config.HarnessClaude {
		c := cfg.Claude
		return pipeline.ClaudeAgents{
			Binary: c.Binary, BinaryArgs: c.Args,
			Model: c.Model, Effort: c.Effort, PermissionMode: c.PermissionMode,
			AgentsFile: target.AgentsFile(), PluginDir: target.PluginDir(),
			Env: c.Environ(), Settings: c.Settings,
		}
	}
	o := cfg.Omp
	return pipeline.RPCAgents{Binary: o.Binary, Model: o.Model, Thinking: o.Thinking, ApprovalMode: o.ApprovalMode}
}

func openStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	return store.Open(ctx, filepath.Join(cfg.Paths.StateDir, "delamain.db"))
}

func printResult(w io.Writer, res pipeline.Result) {
	icon := map[pipeline.Outcome]string{
		pipeline.OutcomeApproved: "✅", pipeline.OutcomeExhausted: "⚠️ ", pipeline.OutcomeHalted: "❌", pipeline.OutcomeNeedsInfo: "❓",
	}[res.Outcome]
	fmt.Fprintf(w, "%s %s", icon, res.Outcome)
	if res.Reason != "" {
		fmt.Fprintf(w, " — %s", res.Reason)
	}
	fmt.Fprintln(w)
	if res.Branch != "" {
		fmt.Fprintf(w, "   branch:     %s (base %s)\n   worktree:   %s\n   iterations: %d · commits: %d\n", res.Branch, res.Base, res.Worktree, res.Iterations, len(res.Commits))
	}
	if res.PRURL != "" {
		fmt.Fprintf(w, "   PR/MR:      %s%s\n", res.PRURL, map[bool]string{true: " (draft)"}[res.Draft])
	}
	if res.Delivery != "" {
		fmt.Fprintf(w, "   delivery:   %s\n", res.Delivery)
	}
	if res.Budget != "" {
		fmt.Fprintf(w, "   usage:      %s\n", res.Budget)
	}
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "   ⚠️  %s\n", warn)
	}
	if res.ReportPath != "" {
		fmt.Fprintf(w, "   report:     %s\n", res.ReportPath)
	}
}

// IsExit unwraps an ExitError code.
func IsExit(err error) (int, bool) {
	var e ExitError
	if errors.As(err, &e) {
		return e.Code, true
	}
	return 0, false
}
