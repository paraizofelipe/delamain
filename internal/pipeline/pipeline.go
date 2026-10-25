// Package pipeline runs one task through plan → [implement → verify → review]×N.
//
// Agents are omp sessions driven over RPC; the orchestrator only trusts what it can check
// itself: artifact files, the omp session, git state and the checks it runs. A Display (herdr)
// may show the run, but nothing depends on it.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/agent"
	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/complexity"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/omp"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// Git is the subset of gitx the pipeline uses.
type Git interface {
	MainWorktree(ctx context.Context, dir string) (string, error)
	ResolveBase(ctx context.Context, repo string) (string, error)
	BranchExists(ctx context.Context, repo, branch string) bool
	Head(ctx context.Context, dir string) (string, error)
	CurrentBranch(ctx context.Context, dir string) (string, error)
	Status(ctx context.Context, dir string) (string, error)
	CommitsSince(ctx context.Context, dir, base string) ([]string, error)
	ChangedFiles(ctx context.Context, dir, base string) ([]string, error)
	AddWorktree(ctx context.Context, repo, path, branch, base string) error
	AddDetachedWorktree(ctx context.Context, repo, path, ref string) error
	AddUntrustedWorktree(ctx context.Context, repo, path, ref string) error
	UntrustedStatus(ctx context.Context, dir string) (string, error)
	CheckoutFiles(ctx context.Context, dir, ref string, files []string) error
	DiscardEmptyWork(ctx context.Context, repo, worktree, branch, base string) error
	RemoveWorktree(ctx context.Context, repo, path string) error
	Push(ctx context.Context, dir, branch, base string) error
	RemoteBranchExists(ctx context.Context, repo, branch string) (bool, error)
	FetchPRHead(ctx context.Context, repo string, kind forge.Kind, n int) (string, error)
	FetchRef(ctx context.Context, repo, ref string) error
	ShowFile(ctx context.Context, repo, ref, path string) ([]byte, error)
	ChangedLines(ctx context.Context, dir, base string) (map[string][]gitx.LineRange, error)
	DeleteRef(ctx context.Context, repo, ref string) error
	RemoteSHA(ctx context.Context, repo, branch string) (string, error)
	AddWorktreeOnBranch(ctx context.Context, repo, path, branch, base string) error
	IsAncestor(ctx context.Context, dir, ancestor, ref string) (bool, error)
	CommitMessages(ctx context.Context, dir, base string) ([]string, error)
	PushFastForward(ctx context.Context, dir, branch, base, expected string) error
}

type Outcome string

const (
	OutcomeApproved  Outcome = "approved"   // review approved within the iteration budget
	OutcomeExhausted Outcome = "exhausted"  // max iterations reached without approval
	OutcomeHalted    Outcome = "halted"     // stopped by a guardrail or an infrastructure failure
	OutcomeNeedsInfo Outcome = "needs_info" // preflight rejected the task; nothing was created
	OutcomeReviewed  Outcome = "reviewed"   // the PR review is posted (or was already, comment by comment)
	OutcomeSkipped   Outcome = "skipped"    // nothing to review: PR not open or head already reviewed
	OutcomeAnswered  Outcome = "answered"   // the pending review threads of the PR got their replies
)

type Result struct {
	RunID       int64
	Outcome     Outcome
	Reason      string
	Branch      string
	Base        string
	Worktree    string
	WorkspaceID string
	Iterations  int
	Complexity  string // the plan's assessment, e.g. "high (12 arquivos; sinais altos: change, files)"
	Commits     []string
	RunDir      string
	ReportPath  string
	History     []Iteration
	PRURL       string
	Draft       bool
	// Delivery explains a skipped delivery; Warnings collect non-fatal forge failures.
	Delivery string
	Warnings []string
	// Budget is the run's spend, e.g. "167K de 2M tokens · US$ 0.67".
	Budget string
	// CommandSources says where each gate command came from: repo, detectado or planner.
	CommandSources map[string]string
	// Review is what a PR review posted; nil for issue runs.
	Review *ReviewSummary
}

// Iteration records what happened in one implement → verify → review round.
type Iteration struct {
	N        int
	Checks   []CheckResult
	Verified bool
	Review   *artifacts.Review
	Feedback string
}

// startTimeout bounds an omp session's startup until its RPC ready frame.
const startTimeout = 60 * time.Second

type Runner struct {
	Cfg config.Config
	// Display shows runs while they happen (herdr); nil runs them headless.
	Display Display
	Agents  Agents
	// Checks runs the gates (default ExecChecks).
	Checks        Checks
	Git           Git
	Store         *store.Store
	Prompts       *prompts.Set
	PromptsTarget prompts.Target
	// Forge opens the PR/MR and updates the issue; nil keeps the work local (repo without origin).
	Forge forge.Forge
	// ForgeUser is the login Forge posts as; only its comments' markers count as already posted.
	ForgeUser string
	Log       *slog.Logger
	// LookPath finds optional tools such as gitleaks (default exec.LookPath).
	LookPath func(string) (string, error)
	// Alive reports whether a run's owner process still exists (default: signal 0).
	Alive func(pid int) bool
	// Notify shows desktop toasts; nil sends none.
	Notify notify.Notifier
	// BudgetInterval paces the budget watcher during agent turns (default 10s).
	BudgetInterval time.Duration
	// SubagentWait is how long a step waits before giving its subagents another turn to
	// report back (default 30s); see stepArtifact.
	SubagentWait time.Duration
	// StopPoll paces how often a run checks whether `delamain stop` asked it to stop (default 2s).
	StopPoll time.Duration
	// Created is called once an issue run is recorded, before any of its work: the queue lets the
	// issue go only then, so a run that never got recorded leaves it queued. nil does nothing.
	Created func(ctx context.Context, rec store.Run)
}

// halt is a guardrail stop: the run is recorded with this reason instead of failing the process.
type halt struct{ reason string }

func (h halt) Error() string { return h.reason }

func haltf(format string, a ...any) error { return halt{fmt.Sprintf(format, a...)} }

// run is the mutable state of one execution.
type run struct {
	rec    store.Run
	task   prompts.Issue
	repo   prompts.Repo
	dir    string
	agents map[string]Agent // role -> running session
	view   View             // nil without a display
	// log also writes the run's own logs/delamain.log, read by `delamain logs`.
	log      *slog.Logger
	closeLog func()
	planPath string
	planJSON string
	plan     artifacts.Plan
	res      *Result
	issue    int // forge issue number; 0 for local tasks
	// maxIter and perIssue start global; applyComplexity sizes them once the plan is classified.
	maxIter  int
	perIssue float64
	// Read from the base checkout before any agent runs, so agents cannot weaken them.
	repoCfg gates.RepoConfig
	stack   gates.Stack
	cmds    artifacts.Commands
	budget  budget.Status
	// recovered marks a run finalized by Recover after its process died.
	recovered bool
	// pr is set on PR review runs only.
	pr *prRun
	// agentDir is the agents' cwd when it must not be the worktree (default: the worktree).
	agentDir string
	// confine restricts agents that read third-party code; nil for issue runs.
	confine *Confinement
}

// Run executes the task end to end. A non-nil error means the run could not even be recorded;
// guardrail stops are reported through Result.Outcome == OutcomeHalted.
func (r *Runner) Run(ctx context.Context, t Task) (Result, error) {
	res := Result{}
	problems := t.Preflight()
	if len(problems) == 0 {
		problems = r.blockers(ctx, t)
	}
	if len(problems) > 0 {
		res.Outcome, res.Reason = OutcomeNeedsInfo, strings.Join(problems, "; ")
		r.needsInfo(ctx, t, problems, &res)
		return res, nil
	}
	repo, err := r.Git.MainWorktree(ctx, t.Repo)
	if err != nil {
		return res, fmt.Errorf("repository: %w", err)
	}
	if err := r.ensurePrompts(); err != nil {
		return res, err
	}

	day, err := r.meter().Today(ctx)
	if err != nil {
		return res, err
	}
	if day.Level == budget.Hard {
		res.Outcome, res.Reason = OutcomeHalted, day.Reason+"; nenhuma run iniciada"
		return res, nil
	}

	rec, err := r.Store.CreateRun(ctx, repo, t.Ref, t.Title, string(r.Cfg.Agent.Harness), r.Cfg.Pipeline.MaxIterations, r.Cfg.Budget.PerIssue, store.KindIssue)
	if err != nil {
		return res, err
	}
	res.RunID = rec.ID
	if r.Created != nil {
		r.Created(ctx, rec)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer r.watchStop(ctx, rec.ID, cancel)()
	ru := &run{
		rec:      rec,
		task:     prompts.Issue{Ref: t.Ref, Title: t.Title, Body: t.Body, URL: t.URL, Kind: t.Kind},
		dir:      r.runDir(repo, rec.ID),
		agents:   map[string]Agent{},
		res:      &res,
		issue:    t.Issue,
		maxIter:  r.Cfg.Pipeline.MaxIterations,
		perIssue: r.Cfg.Budget.PerIssue,
		budget:   day,
	}
	ru.planPath, ru.planJSON = filepath.Join(ru.dir, "plan.md"), filepath.Join(ru.dir, "plan.json")
	res.RunDir = ru.dir
	r.openLog(ru)
	defer ru.closeLog()
	r.notify(ctx, ru.log, notify.FormatStarted(notifyRun(ru, t, rec.State)))
	r.issueUpdate(ctx, t, &res, []string{r.Cfg.Forge.Labels.InProgress}, []string{r.Cfg.Forge.Labels.Ready}, "")

	defer r.cleanup(context.WithoutCancel(ctx), ru) // safety net; finish normally stops agents first
	err = r.execute(ctx, ru, repo, t)

	var h halt
	switch {
	case errors.Is(context.Cause(ctx), ErrStopped): // a stopped step fails as a halt or a cancel
		ru.log.Info("run stopped by request", "run", rec.ID)
		res.Outcome, res.Reason = OutcomeHalted, ErrStopped.Error()
	case err == nil:
	case errors.As(err, &h):
		res.Outcome, res.Reason = OutcomeHalted, h.reason
	default:
		res.Outcome, res.Reason = OutcomeHalted, err.Error()
	}
	return res, r.finish(ctx, ru, t)
}

// finish is steps 10–12 for any run that got a record, including recovered ones: final budget
// tally, agents stopped, delivery, removal of empty work, issue update and the report.
func (r *Runner) finish(ctx context.Context, ru *run, t Task) error {
	ctx = context.WithoutCancel(ctx)
	state := ru.rec.State      // record moves it to recorded; the halted toast shows where it stopped
	_ = r.checkBudget(ctx, ru) // final tally; the level no longer matters
	r.cleanup(ctx, ru)         // agents stop before anything touches their worktree
	if ru.repo.Worktree != "" {
		ru.res.Commits, _ = r.Git.CommitsSince(ctx, ru.repo.Worktree, ru.repo.Base)
	}
	r.deliver(ctx, ru)
	r.discardEmpty(ctx, ru)
	r.announce(ctx, ru, t)
	err := r.record(ctx, ru) // writes report.md before a toast offers to open it
	if !ru.recovered {       // the recover toast sums recovered runs up
		r.notify(ctx, ru.log, outcomeToast(ru, t, state))
	}
	return err
}

func (r *Runner) runDir(repo string, id int64) string { return RunDir(r.Cfg.Paths.StateDir, repo, id) }

// RunDir holds a run's artifacts, sessions and logs, outside its worktree.
func RunDir(stateDir, repo string, id int64) string {
	return filepath.Join(stateDir, "runs", filepath.Base(repo), fmt.Sprintf("%d", id))
}

// installMu serializes prompt installs between concurrent runs of one daemon.
var installMu sync.Mutex

// ensurePrompts installs delamain's namespaced prompts only when they drifted: an install
// rewrites skill directories that agents of other running issues may be reading.
func (r *Runner) ensurePrompts() error {
	installMu.Lock()
	defer installMu.Unlock()
	missing, outdated, err := r.Prompts.Drift(r.PromptsTarget)
	if err != nil {
		return err
	}
	if len(missing)+len(outdated) == 0 {
		return nil
	}
	_, err = r.Prompts.Install(r.PromptsTarget)
	return err
}

// discardEmpty removes the worktree and branch of a run that ended without commits, so a halted
// run never blocks the next attempt. Uncommitted changes are kept and reported.
func (r *Runner) discardEmpty(ctx context.Context, ru *run) {
	res := ru.res
	if ru.repo.Worktree == "" || len(res.Commits) > 0 || res.PRURL != "" {
		return
	}
	if err := r.Git.DiscardEmptyWork(ctx, ru.rec.Repo, ru.repo.Worktree, ru.repo.Branch, ru.repo.Base); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("worktree %s preservada: %v", ru.repo.Worktree, err))
		return
	}
	res.Delivery += "; worktree e branch vazias removidas"
}

func (r *Runner) setState(ctx context.Context, ru *run, s store.RunState) error {
	ru.rec.State = s
	ru.log.Info("state", "run", ru.rec.ID, "state", s, "iteration", ru.rec.Iteration)
	r.reportMeta(ctx, ru)
	return r.Store.UpdateRun(ctx, ru.rec)
}

func (r *Runner) execute(ctx context.Context, ru *run, repo string, t Task) error {
	if err := r.setState(ctx, ru, store.StateSetup); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(ru.dir, "sessions"), 0o700); err != nil {
		return err
	}
	if err := r.setup(ctx, ru, repo, t); err != nil {
		return err
	}
	if err := r.loadGates(ru); err != nil {
		return err
	}

	if err := r.setState(ctx, ru, store.StatePlan); err != nil {
		return err
	}
	plan, err := stepArtifact[artifacts.Plan](ctx, r, ru, "planner", "plan", r.stageData(ru, 0, ru.planJSON, ""))
	if err != nil {
		return err
	}
	ru.plan = plan
	if err := r.applyComplexity(ctx, ru); err != nil {
		return err
	}
	r.stopAgent(ctx, ru, "planner") // its job is done: an idle agent is only a failure point
	ru.cmds, ru.res.CommandSources = gates.Effective(ru.repoCfg.Commands, ru.stack.Commands, plan.Commands)
	ru.log.Info("gate commands", "run", ru.rec.ID, "stack", ru.stack.Name, "sources", ru.res.CommandSources)

	feedback, digest, why := "", "", ""
	seenBlockers, diverging := map[string]bool{}, 0
	for iter := 1; iter <= ru.maxIter; iter++ {
		it := Iteration{N: iter, Feedback: feedback}
		ru.rec.Iteration, ru.res.Iterations = iter, iter
		iterDir := filepath.Join(ru.dir, fmt.Sprintf("iter-%d", iter))
		if err := os.MkdirAll(iterDir, 0o700); err != nil {
			return err
		}
		if iter > 1 {
			r.notify(ctx, ru.log, iterationToast(ru, t, why))
		}

		if err := r.setState(ctx, ru, store.StateImplement); err != nil {
			return err
		}
		if _, err := stepArtifact[artifacts.Impl](ctx, r, ru, "implementer", "implement", r.stageData(ru, iter, filepath.Join(iterDir, "impl.json"), feedback)); err != nil {
			return err
		}

		if err := r.setState(ctx, ru, store.StateVerify); err != nil {
			return err
		}
		checks, err := r.verify(ctx, ru, iterDir)
		if err != nil {
			return err
		}
		it.Checks, it.Verified = checks, allPassed(checks)
		if !it.Verified {
			feedback, why = verifyFeedback(checks), verifyReason(checks)
			ru.res.History = append(ru.res.History, it)
			continue
		}

		if err := r.setState(ctx, ru, store.StateReview); err != nil {
			return err
		}
		review, err := r.review(ctx, ru, iter, filepath.Join(iterDir, "review.json"), checks, digest)
		if err != nil {
			return err
		}
		it.Review = &review
		ru.res.History = append(ru.res.History, it)
		if review.EffectiveVerdict() == artifacts.VerdictApprove {
			ru.res.Outcome = OutcomeApproved
			return r.setState(ctx, ru, store.StateDeliver)
		}
		feedback, digest, why = reviewFeedback(review), prevReviewDigest(review), reviewReason(review)
		if markBlockers(seenBlockers, review) {
			diverging++
		} else {
			diverging = 0
		}
		if diverging >= divergingReviews {
			ru.log.Info("review divergente", "run", ru.rec.ID, "reviews", diverging)
			ru.res.Outcome = OutcomeExhausted
			ru.res.Reason = fmt.Sprintf("review divergente: %d iterações seguidas com bloqueantes inéditos", diverging)
			return r.setState(ctx, ru, store.StateDeliverDraft)
		}
	}
	ru.res.Outcome = OutcomeExhausted
	ru.res.Reason = fmt.Sprintf("%d iterações sem aprovação", ru.maxIter)
	return r.setState(ctx, ru, store.StateDeliverDraft)
}

// divergingReviews is how many reviews in a row may raise only brand-new blockers before the run
// is handed to a human. A reviewer that never repeats itself is auditing one layer deeper each
// time, not converging: the iterations cost full budget and the implementer never learns when it
// is done. Run 12 of this repository burned five iterations that way.
const divergingReviews = 3

// markBlockers records the review's blocking findings and reports whether every one of them is
// new to the run. Findings are compared by findingKey, not by message.
func markBlockers(seen map[string]bool, r artifacts.Review) bool {
	allNew := true
	for _, a := range r.Axes {
		for _, f := range a.Findings {
			if !f.Blocking() {
				continue
			}
			k := findingKey(a.Axis, f)
			if seen[k] {
				allNew = false
			}
			seen[k] = true
		}
	}
	return allNew
}

// plannedFilesWarning is the plan size above which a phase is worth splitting: past it the review
// grows a diff it cannot audit in one pass.
const plannedFilesWarning = 10

// applyComplexity classifies the plan and sizes the run's iterations and per-issue budget by its
// level. The budget is checked right away: planning may already have spent a smaller profile.
func (r *Runner) applyComplexity(ctx context.Context, ru *run) error {
	a := complexity.Classify(ru.plan)
	ru.maxIter, ru.perIssue = r.Cfg.Limits(string(a.Level))
	ru.rec.MaxIterations, ru.rec.PerIssue, ru.rec.Complexity = ru.maxIter, ru.perIssue, string(a.Level)
	ru.res.Complexity = a.Summary()
	ru.log.Info("complexity", "run", ru.rec.ID, "assessment", ru.res.Complexity, "max_iterations", ru.maxIter, "per_issue", ru.perIssue)
	if a.Files > plannedFilesWarning {
		ru.res.Warnings = append(ru.res.Warnings, fmt.Sprintf(
			"o plano toca %d arquivos de produção (acima de %d): considere dividir a issue em fases menores", a.Files, plannedFilesWarning))
	}
	if err := r.Store.UpdateRun(ctx, ru.rec); err != nil {
		return err
	}
	return r.checkBudget(ctx, ru)
}

func (r *Runner) setup(ctx context.Context, ru *run, repo string, t Task) error {
	base, err := r.Git.ResolveBase(ctx, repo)
	if err != nil {
		return err
	}
	branch := gitx.BranchName(t.Kind, t.Ref, t.Title)
	if err := gitx.GuardWorkBranch(branch, base); err != nil {
		return halt{err.Error()}
	}
	if r.Git.BranchExists(ctx, repo, branch) {
		return haltf("a branch %s já existe; remova-a ou finalize o trabalho anterior", branch)
	}
	if err := r.checkForge(ctx, repo, branch); err != nil {
		return err
	}
	wtPath := filepath.Join(r.Cfg.Paths.WorktreesDir, filepath.Base(repo), branch)
	if _, err := os.Stat(wtPath); err == nil {
		return haltf("o caminho da worktree %s já existe", wtPath)
	}

	if err := r.Git.AddWorktree(ctx, repo, wtPath, branch, base); err != nil {
		return err
	}
	ru.repo = prompts.Repo{Worktree: wtPath, Branch: branch, Base: base}
	ru.rec.Branch, ru.rec.Worktree, ru.rec.Base = branch, wtPath, base
	ru.res.Branch, ru.res.Base, ru.res.Worktree = branch, base, wtPath
	ru.log.Info("worktree", "run", ru.rec.ID, "path", wtPath, "branch", branch, "base", base)
	r.openView(ctx, ru, t.Ref+" "+t.Title)
	return r.Store.UpdateRun(ctx, ru.rec)
}

// openView shows the run on the display, if any. Display only: a failure never stops the run.
func (r *Runner) openView(ctx context.Context, ru *run, label string) {
	if r.Display == nil {
		return
	}
	v, err := r.Display.Open(ctx, ru.repo.Worktree, label, r.checksLog(ru))
	if err != nil {
		ru.log.Warn("display", "err", err)
	}
	if v != nil {
		ru.view = v
		ru.rec.Workspace, ru.res.WorkspaceID = v.ID(), v.ID()
	}
}

// viewDo runs a display step when the run has a view. Display only: failures are logged.
func (ru *run) viewDo(what string, fn func(View) error) {
	if ru.view == nil {
		return
	}
	if err := fn(ru.view); err != nil {
		ru.log.Warn("display: "+what, "err", err)
	}
}

func (r *Runner) logsDir(ru *run) string   { return filepath.Join(ru.dir, "logs") }
func (r *Runner) checksLog(ru *run) string { return filepath.Join(r.logsDir(ru), "checks.log") }

// openLog tees the orchestrator's log of this run into logs/delamain.log. Without the file the
// run still logs to r.Log.
func (r *Runner) openLog(ru *run) {
	ru.log, ru.closeLog = r.Log, func() {}
	if err := os.MkdirAll(r.logsDir(ru), 0o700); err != nil {
		r.Log.Warn("could not create the run log", "err", err)
		return
	}
	f, err := os.OpenFile(filepath.Join(r.logsDir(ru), "delamain.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.Log.Warn("could not create the run log", "err", err)
		return
	}
	ru.log = slog.New(teeHandler{r.Log.Handler(), newLineHandler(f)})
	ru.closeLog = func() { _ = f.Close() }
}

func (r *Runner) startAgent(ctx context.Context, ru *run, role string) error {
	sessDir := filepath.Join(ru.dir, "sessions", role)
	logDir := r.logsDir(ru)
	for _, d := range []string{sessDir, logDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	logPath := filepath.Join(logDir, role+".log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		return err
	}
	ru.viewDo("show "+role, func(v View) error { return v.Show(ctx, role, logPath) })
	dir := ru.repo.Worktree
	if ru.agentDir != "" {
		dir = ru.agentDir
	}
	model, reasoning := r.Cfg.AgentOverride(role)
	agent, err := r.Agents.Start(ctx, AgentOptions{
		Name: fmt.Sprintf("%s-r%d", role, ru.rec.ID), Dir: dir,
		SessionDir: sessDir, SystemPrompt: r.PromptsTarget.RolePath(role),
		// Only the orchestrator publishes work: the agent gets an env where push fails.
		Env:     gitx.AgentEnv(),
		LogPath: logPath, ErrPath: filepath.Join(logDir, role+".stderr"),
		Confine: ru.confine, Model: model, Reasoning: reasoning,
	})
	if err != nil {
		return haltf("o agente %s não subiu: %v", role, err)
	}
	ru.agents[role] = agent
	ru.log.Info("agent started", "run", ru.rec.ID, "role", role, "pid", agent.PID(), "model", model)
	if err := r.Store.StartAgent(ctx, store.Agent{RunID: ru.rec.ID, Role: role, PID: agent.PID(), LogPath: logPath, SessionDir: sessDir}); err != nil {
		ru.log.Warn("could not record agent", "role", role, "err", err)
	}
	return nil
}

// prompt renders a stage, submits it and proves the turn ended without a provider error.
func (r *Runner) prompt(ctx context.Context, ru *run, role, stage string, data prompts.StageData) error {
	text, err := r.Prompts.Render(stage, data)
	if err != nil {
		return err
	}
	return r.send(ctx, ru, role, text)
}

func (r *Runner) send(ctx context.Context, ru *run, role, text string) error {
	if _, ok := ru.agents[role]; !ok {
		if err := r.startAgent(ctx, ru, role); err != nil {
			return err
		}
	}
	timeout := r.Cfg.Pipeline.PromptTimeout
	stop := r.watchBudget(ctx, ru, ru.agents[role])
	turn, err := ru.agents[role].Prompt(ctx, text, timeout)
	if w := stop(); w.tripped {
		ru.budget, ru.res.Budget = w.status, w.status.Summary()
		return haltf("%s (%s interrompido no meio do turno)", w.status.Reason, role)
	}
	if errors.Is(err, agent.ErrTimeout) {
		return haltf("o %s não terminou o turno em %s; turno abortado", role, timeout)
	}
	if err != nil {
		return haltf("agente %s: %v", role, err)
	}
	if turn.StopReason == "" {
		// The final message never reached us (e.g. a frame over the transport limit): the
		// session file still has it.
		sess, err := omp.LatestSession(filepath.Join(ru.dir, "sessions", role))
		if err != nil {
			return haltf("sessão do %s não encontrada: %v", role, err)
		}
		last, err := omp.LastAssistant(sess)
		if err != nil {
			return haltf("sessão do %s sem resposta: %v", role, err)
		}
		turn = agent.Turn{StopReason: last.StopReason, ErrorMessage: last.ErrorMessage, Model: last.Model}
	}
	switch turn.StopReason {
	case "error":
		return haltf("erro do provider no %s (%s): %s", role, turn.Model, truncate(turn.ErrorMessage, 300))
	case "aborted":
		return haltf("o turno do %s foi abortado", role)
	}
	return r.checkBudget(ctx, ru)
}

const (
	// defaultSubagentWait paces the turns a step gets while its subagents report back.
	defaultSubagentWait = 30 * time.Second
	// subagentWaits is how many further turns a step gets while its subagents may still be
	// running. With the default wait that covers about four minutes, past the usual axis.
	subagentWaits = 8
)

// pause waits SubagentWait, or returns early if the run is cancelled.
func (r *Runner) pause(ctx context.Context) error {
	d := r.SubagentWait
	if d <= 0 {
		d = defaultSubagentWait
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type validated interface{ Validate() error }

// stepArtifact runs a stage and reads its artifact, allowing one corrective prompt.
//
// An artifact that is merely absent does not mean the step failed: Claude Code's Agent tool
// puts a subagent in the background unless the call says otherwise, and hands its result to the
// main session only in a later turn, so the turn can end with the axes still working. Each
// further turn delivers the notifications that landed meanwhile, which is why the step is given
// several before its artifact is called missing. A malformed artifact skips the wait: the step
// did produce a file, so only the single correction turn applies.
func stepArtifact[T validated](ctx context.Context, r *Runner, ru *run, role, stage string, data prompts.StageData) (T, error) {
	var zero T
	if err := r.prompt(ctx, ru, role, stage, data); err != nil {
		return zero, err
	}
	v, err := artifacts.Read[T](data.ArtifactPath)
	if err == nil {
		return v, nil
	}
	for i := 0; errors.Is(err, fs.ErrNotExist) && i < subagentWaits; i++ {
		ru.log.Info("artifact missing, waiting for the step's subagents",
			"stage", stage, "attempt", i+1, "of", subagentWaits, "path", data.ArtifactPath)
		if werr := r.pause(ctx); werr != nil {
			return zero, werr
		}
		wait := fmt.Sprintf("O artefato `%s` ainda não existe. Se os seus subagentes ainda estão rodando, espere o resultado deles e **não** invente achado nenhum. Quando todos tiverem respondido, escreva o arquivo no formato pedido no passo e responda apenas: `DONE %s`. Se já terminaram, escreva o arquivo agora.",
			data.ArtifactPath, data.ArtifactPath)
		if serr := r.send(ctx, ru, role, wait); serr != nil {
			return zero, serr
		}
		if v, err = artifacts.Read[T](data.ArtifactPath); err == nil {
			return v, nil
		}
	}
	ru.log.Warn("invalid artifact, asking for a correction", "stage", stage, "err", err)
	fix := fmt.Sprintf("O artefato `%s` está ausente ou inválido:\n\n%s\n\nCorrija o arquivo no mesmo caminho, exatamente no formato pedido no passo anterior, e responda apenas: `DONE %s`",
		data.ArtifactPath, truncate(err.Error(), 1500), data.ArtifactPath)
	if err := r.send(ctx, ru, role, fix); err != nil {
		return zero, err
	}
	if v, err = artifacts.Read[T](data.ArtifactPath); err != nil {
		return zero, haltf("passo %s sem artefato válido após correção: %v", stage, err)
	}
	return v, nil
}

func (r *Runner) stageData(ru *run, iter int, artifact, feedback string) prompts.StageData {
	return prompts.StageData{
		Issue: ru.task, Repo: ru.repo, Iteration: iter, MaxIterations: ru.maxIter,
		ArtifactPath: artifact, PlanPath: ru.planPath, PlanJSONPath: ru.planJSON,
		Feedback: feedback, BudgetLeft: ru.budget.Left(),
	}
}

func (r *Runner) review(ctx context.Context, ru *run, iter int, artifact string, checks []CheckResult, prev string) (artifacts.Review, error) {
	head, err := r.Git.Head(ctx, ru.repo.Worktree)
	if err != nil {
		return artifacts.Review{}, err
	}
	status, err := r.Git.Status(ctx, ru.repo.Worktree)
	if err != nil {
		return artifacts.Review{}, err
	}
	data := r.stageData(ru, iter, artifact, "")
	data.Verify, data.PrevReview = verifySummary(checks), prev
	review, err := stepArtifact[artifacts.Review](ctx, r, ru, "reviewer", "review", data)
	if err != nil {
		return review, err
	}
	// The reviewer is read-only by contract; prove it.
	head2, _ := r.Git.Head(ctx, ru.repo.Worktree)
	status2, _ := r.Git.Status(ctx, ru.repo.Worktree)
	if head2 != head || status2 != status {
		return review, haltf("o reviewer alterou a worktree (HEAD %s→%s); revisão descartada", short(head), short(head2))
	}
	return review, nil
}

func (r *Runner) stopAgent(ctx context.Context, ru *run, role string) {
	agent, ok := ru.agents[role]
	if !ok {
		return
	}
	state := store.AgentDone
	if err := agent.Close(); err != nil {
		state = store.AgentFailed
		ru.log.Warn("could not stop agent", "role", role, "err", err)
	}
	delete(ru.agents, role)
	ru.log.Info("agent stopped", "run", ru.rec.ID, "role", role, "state", state)
	if err := r.Store.EndAgent(ctx, ru.rec.ID, role, state); err != nil {
		ru.log.Warn("could not record agent", "role", role, "err", err)
	}
	ru.viewDo("done "+role, func(v View) error { return v.Done(ctx, role) })
}

// cleanup stops the run's agents and closes its view (step 12). The git worktree stays:
// it holds the branch that will become the MR. Idempotent.
func (r *Runner) cleanup(ctx context.Context, ru *run) {
	for role := range ru.agents {
		r.stopAgent(ctx, ru, role)
	}
	ru.viewDo("close", func(v View) error { return v.Close(ctx) })
	ru.view = nil
}

func (r *Runner) record(ctx context.Context, ru *run) error {
	res := ru.res
	if ru.dir != "" {
		if err := os.MkdirAll(ru.dir, 0o700); err == nil {
			res.ReportPath = filepath.Join(ru.dir, "report.md")
			report := renderReport(*res, ru.task)
			if ru.pr != nil {
				report = renderReviewReport(*res, ru.pr)
			}
			if err := os.WriteFile(res.ReportPath, []byte(report), 0o600); err != nil {
				ru.log.Warn("could not write report", "err", err)
			}
		}
	}
	if res.Outcome == OutcomeHalted {
		ru.rec.State = store.StateHalted
		_ = r.Store.UpdateRun(ctx, ru.rec)
	}
	ru.rec.Reason = string(res.Outcome)
	if res.Reason != "" {
		ru.rec.Reason += ": " + res.Reason
	}
	return r.setState(ctx, ru, store.StateRecorded)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
