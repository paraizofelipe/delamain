// Package pipeline runs one task through plan → [implement → verify → review]×N.
//
// Agents run interactively in herdr panes; the orchestrator only trusts what it can
// check itself: artifact files, the omp session, git state and the checks it runs.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gates"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/omp"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
)

// Herdr is the subset of the herdr client the pipeline drives.
type Herdr interface {
	WorktreeCreate(ctx context.Context, o herdr.WorktreeOptions) (herdr.Worktree, error)
	TabCreate(ctx context.Context, workspace, cwd, label string) (herdr.Tab, error)
	TabRename(ctx context.Context, tab, label string) error
	AgentStart(ctx context.Context, name, kind, pane string, timeout time.Duration, agentArgs ...string) (herdr.Agent, error)
	AgentPrompt(ctx context.Context, target, text string, timeout time.Duration) (herdr.Agent, error)
	AgentWait(ctx context.Context, target string, timeout time.Duration) (herdr.Agent, error)
	AgentSendKeys(ctx context.Context, target string, keys ...string) error
	AgentList(ctx context.Context) ([]herdr.Agent, error)
	WorkspaceClose(ctx context.Context, workspaceID string) error
	WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, seq int64, tokens map[string]string) error
	PaneRun(ctx context.Context, pane, command string) error
	PaneWaitOutput(ctx context.Context, pane, regex string, timeout time.Duration) (string, error)
}

// Git is the subset of gitx the pipeline uses.
type Git interface {
	Toplevel(ctx context.Context, dir string) (string, error)
	ResolveBase(ctx context.Context, repo string) (string, error)
	BranchExists(ctx context.Context, repo, branch string) bool
	Head(ctx context.Context, dir string) (string, error)
	CurrentBranch(ctx context.Context, dir string) (string, error)
	Status(ctx context.Context, dir string) (string, error)
	CommitsSince(ctx context.Context, dir, base string) ([]string, error)
	ChangedFiles(ctx context.Context, dir, base string) ([]string, error)
	AddDetachedWorktree(ctx context.Context, repo, path, ref string) error
	CheckoutFiles(ctx context.Context, dir, ref string, files []string) error
	DiscardEmptyWork(ctx context.Context, repo, worktree, branch, base string) error
	RemoveWorktree(ctx context.Context, repo, path string) error
	Push(ctx context.Context, dir, branch, base string) error
	RemoteBranchExists(ctx context.Context, repo, branch string) (bool, error)
}

type Outcome string

const (
	OutcomeApproved  Outcome = "approved"   // review approved within the iteration budget
	OutcomeExhausted Outcome = "exhausted"  // max iterations reached without approval
	OutcomeHalted    Outcome = "halted"     // stopped by a guardrail or an infrastructure failure
	OutcomeNeedsInfo Outcome = "needs_info" // preflight rejected the task; nothing was created
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
}

// Iteration records what happened in one implement → verify → review round.
type Iteration struct {
	N        int
	Checks   []CheckResult
	Verified bool
	Review   *artifacts.Review
	Feedback string
}

const (
	roleTimeout = 60 * time.Second
	kindOmp     = "omp"
)

type Runner struct {
	Cfg           config.Config
	Herdr         Herdr
	Git           Git
	Store         *store.Store
	Prompts       *prompts.Set
	PromptsTarget prompts.Target
	// Forge opens the PR/MR and updates the issue; nil keeps the work local (repo without origin).
	Forge forge.Forge
	Log   *slog.Logger
	// Nonce makes check sentinels unguessable; overridable in tests.
	Nonce func() string
	// LookPath finds optional tools such as gitleaks (default exec.LookPath).
	LookPath func(string) (string, error)
	// Alive reports whether a run's owner process still exists (default: signal 0).
	Alive func(pid int) bool
	// BudgetInterval paces the budget watcher during agent turns (default 10s).
	BudgetInterval time.Duration
	// RetryInterval paces retries while a fresh pane's shell is still starting (default 1s).
	RetryInterval time.Duration
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
	agents map[string]string // role -> agent name
	panes  map[string]string // role -> pane id
	tabs   map[string]string // role -> tab id
	// workspace is set only after setup proves it is ours, so cleanup never closes someone else's.
	workspace string
	planPath  string
	planJSON  string
	plan      artifacts.Plan
	res       *Result
	issue     int // forge issue number; 0 for local tasks
	maxIter   int
	// Read from the base checkout before any agent runs, so agents cannot weaken them.
	repoCfg gates.RepoConfig
	stack   gates.Stack
	cmds    artifacts.Commands
	budget  budget.Status
	metaSeq int64
	// recovered marks a run finalized by Recover after its process died.
	recovered bool
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
	repo, err := r.Git.Toplevel(ctx, t.Repo)
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

	rec, err := r.Store.CreateRun(ctx, repo, t.Ref, t.Title)
	if err != nil {
		return res, err
	}
	res.RunID = rec.ID
	ru := &run{
		rec:     rec,
		task:    prompts.Issue{Ref: t.Ref, Title: t.Title, Body: t.Body, URL: t.URL, Kind: t.Kind},
		dir:     r.runDir(repo, rec.ID),
		agents:  map[string]string{},
		panes:   map[string]string{},
		tabs:    map[string]string{},
		res:     &res,
		issue:   t.Issue,
		maxIter: r.Cfg.Pipeline.MaxIterations,
		budget:  day,
	}
	ru.planPath, ru.planJSON = filepath.Join(ru.dir, "plan.md"), filepath.Join(ru.dir, "plan.json")
	res.RunDir = ru.dir
	r.issueUpdate(ctx, t, &res, []string{r.Cfg.Forge.Labels.InProgress}, []string{r.Cfg.Forge.Labels.Ready}, "")

	defer r.cleanup(context.WithoutCancel(ctx), ru) // safety net; finish normally stops agents first
	err = r.execute(ctx, ru, repo, t)

	var h halt
	switch {
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
	_ = r.checkBudget(ctx, ru) // final tally; the level no longer matters
	r.cleanup(ctx, ru)         // agents stop before anything touches their worktree
	if ru.repo.Worktree != "" {
		ru.res.Commits, _ = r.Git.CommitsSince(ctx, ru.repo.Worktree, ru.repo.Base)
	}
	r.deliver(ctx, ru)
	r.discardEmpty(ctx, ru)
	r.announce(ctx, ru, t)
	return r.record(ctx, ru)
}

func (r *Runner) runDir(repo string, id int64) string {
	return filepath.Join(r.Cfg.Paths.StateDir, "runs", filepath.Base(repo), fmt.Sprintf("%d", id))
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
	r.Log.Info("state", "run", ru.rec.ID, "state", s, "iteration", ru.rec.Iteration)
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
	r.stopAgent(ctx, ru, "planner") // its job is done: an idle agent is only a failure point
	ru.cmds, ru.res.CommandSources = gates.Effective(ru.repoCfg.Commands, ru.stack.Commands, plan.Commands)
	r.Log.Info("gate commands", "run", ru.rec.ID, "stack", ru.stack.Name, "sources", ru.res.CommandSources)

	feedback := ""
	maxIter := r.Cfg.Pipeline.MaxIterations
	for iter := 1; iter <= maxIter; iter++ {
		it := Iteration{N: iter, Feedback: feedback}
		ru.rec.Iteration, ru.res.Iterations = iter, iter
		iterDir := filepath.Join(ru.dir, fmt.Sprintf("iter-%d", iter))
		if err := os.MkdirAll(iterDir, 0o700); err != nil {
			return err
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
			feedback = verifyFeedback(checks)
			ru.res.History = append(ru.res.History, it)
			continue
		}

		if err := r.setState(ctx, ru, store.StateReview); err != nil {
			return err
		}
		review, err := r.review(ctx, ru, iter, filepath.Join(iterDir, "review.json"), checks)
		if err != nil {
			return err
		}
		it.Review = &review
		ru.res.History = append(ru.res.History, it)
		if review.EffectiveVerdict() == artifacts.VerdictApprove {
			ru.res.Outcome = OutcomeApproved
			return r.setState(ctx, ru, store.StateDeliver)
		}
		feedback = reviewFeedback(review)
	}
	ru.res.Outcome = OutcomeExhausted
	ru.res.Reason = fmt.Sprintf("%d iterações sem aprovação", maxIter)
	return r.setState(ctx, ru, store.StateDeliverDraft)
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

	wt, err := r.Herdr.WorktreeCreate(ctx, herdr.WorktreeOptions{
		Cwd: repo, Branch: branch, Base: base, Path: wtPath, Label: t.Ref + " " + t.Title,
	})
	if err != nil {
		return err
	}
	if wt.Path == "" {
		wt.Path = wtPath
	}
	// herdr returns an already-open workspace for a known worktree, and restores workspaces
	// (with their agents) after a server restart. Never drive panes another run still owns.
	live, err := r.Herdr.AgentList(ctx)
	if err != nil {
		return err
	}
	for _, a := range live {
		if a.WorkspaceID == wt.WorkspaceID {
			return haltf("o herdr reabriu o workspace %s, que ainda tem o agente %s ativo; feche-o antes de rodar de novo", wt.WorkspaceID, a.Name)
		}
	}
	ru.workspace = wt.WorkspaceID
	ru.repo = prompts.Repo{Worktree: wt.Path, Branch: branch, Base: base}
	ru.rec.Branch, ru.rec.Worktree, ru.rec.Base, ru.rec.Workspace = branch, wt.Path, base, wt.WorkspaceID
	ru.res.Branch, ru.res.Base, ru.res.Worktree, ru.res.WorkspaceID = branch, base, wt.Path, wt.WorkspaceID
	if err := r.Store.UpdateRun(ctx, ru.rec); err != nil {
		return err
	}

	// One full-width tab per role: the root tab runs the gates, each agent gets its own tab
	// when it starts (see startAgent).
	ru.panes["checks"], ru.tabs["checks"] = wt.RootPaneID, wt.TabID
	r.renameTab(ctx, ru, "checks", "checks")
	return nil
}

// renameTab labels a role's tab. Display only: a failure never stops the run.
func (r *Runner) renameTab(ctx context.Context, ru *run, role, label string) {
	if ru.tabs[role] == "" {
		return
	}
	if err := r.Herdr.TabRename(ctx, ru.tabs[role], label); err != nil {
		r.Log.Warn("could not rename tab", "tab", ru.tabs[role], "err", err)
	}
}

func (r *Runner) startAgent(ctx context.Context, ru *run, role string) error {
	name := fmt.Sprintf("%s-r%d", role, ru.rec.ID)
	sessDir := filepath.Join(ru.dir, "sessions", role)
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		return err
	}
	tab, err := r.Herdr.TabCreate(ctx, ru.workspace, ru.repo.Worktree, role)
	if err != nil {
		return fmt.Errorf("tab %s: %w", role, err)
	}
	ru.panes[role], ru.tabs[role] = tab.RootPaneID, tab.ID
	// Only the orchestrator publishes work: the agent's shell gets an env where push fails.
	if err := r.Herdr.PaneRun(ctx, ru.panes[role], "export "+strings.Join(gitx.AgentEnv(), " ")); err != nil {
		return fmt.Errorf("agent env %s: %w", role, err)
	}
	o := r.Cfg.Omp
	args := []string{
		"--model", o.Model, "--thinking", o.Thinking, "--approval-mode", o.ApprovalMode,
		"--session-dir", sessDir, "--append-system-prompt", r.PromptsTarget.RolePath(role),
	}
	if err := r.agentStart(ctx, name, ru.panes[role], args); err != nil {
		return fmt.Errorf("start %s: %w", role, err)
	}
	ru.agents[role] = name

	// herdr reported ready during omp's login wizard (M0): prove the agent really answers.
	ready := filepath.Join(ru.dir, "ready-"+role)
	if err := r.prompt(ctx, ru, role, "handshake", prompts.StageData{ArtifactPath: ready}); err != nil {
		return err
	}
	b, err := os.ReadFile(ready) //nolint:gosec // inside the run dir
	if err != nil || strings.TrimSpace(string(b)) != "ok" {
		return haltf("handshake do %s falhou: o agente não criou %s", role, ready)
	}
	return nil
}

// agentStart retries while the pane's shell is still starting: right after a tab or
// worktree create, herdr rejects the start with agent_pane_busy (seen in E2E with zsh).
func (r *Runner) agentStart(ctx context.Context, name, pane string, args []string) error {
	interval := r.RetryInterval
	if interval <= 0 {
		interval = time.Second
	}
	deadline := time.Now().Add(roleTimeout)
	for {
		_, err := r.Herdr.AgentStart(ctx, name, kindOmp, pane, roleTimeout, args...)
		if !herdr.IsCode(err, herdr.CodeAgentPaneBusy) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
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
	name := ru.agents[role]
	timeout := r.Cfg.Pipeline.PromptTimeout
	stop := r.watchBudget(ctx, ru, name)
	agent, err := r.Herdr.AgentPrompt(ctx, name, text, timeout)
	if herdr.IsCode(err, herdr.CodeAgentPromptStalled) || herdr.IsCode(err, herdr.CodeTimeout) {
		// The prompt may have been delivered: never resubmit blindly, wait once more instead.
		r.Log.Warn("prompt not settled, waiting", "agent", name, "err", err)
		agent, err = r.Herdr.AgentWait(ctx, name, timeout)
	}
	if w := stop(); w.tripped {
		ru.budget, ru.res.Budget = w.status, w.status.Summary()
		return haltf("%s (agente %s interrompido no meio do turno)", w.status.Reason, name)
	}
	if err != nil {
		return haltf("agente %s: %v", name, err)
	}
	if agent.Status == herdr.StatusBlocked {
		return haltf("agente %s está bloqueado aguardando aprovação ou resposta", name)
	}
	if agent.Status == herdr.StatusWorking {
		return haltf("agente %s ainda trabalhando após %s", name, timeout)
	}
	sess := agent.Session.Value
	if sess == "" {
		if sess, err = omp.LatestSession(filepath.Join(ru.dir, "sessions", role)); err != nil {
			return haltf("sessão do %s não encontrada: %v", role, err)
		}
	}
	last, err := omp.LastAssistant(sess)
	if err != nil {
		return haltf("sessão do %s sem resposta: %v", role, err)
	}
	if last.Failed() {
		return haltf("erro do provider no %s (%s): %s", role, last.Model, truncate(last.ErrorMessage, 300))
	}
	return r.checkBudget(ctx, ru)
}

type validated interface{ Validate() error }

// stepArtifact runs a stage and reads its artifact, allowing one corrective prompt.
func stepArtifact[T validated](ctx context.Context, r *Runner, ru *run, role, stage string, data prompts.StageData) (T, error) {
	var zero T
	if err := r.prompt(ctx, ru, role, stage, data); err != nil {
		return zero, err
	}
	v, err := artifacts.Read[T](data.ArtifactPath)
	if err == nil {
		return v, nil
	}
	r.Log.Warn("invalid artifact, asking for a correction", "stage", stage, "err", err)
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
		Issue: ru.task, Repo: ru.repo, Iteration: iter, MaxIterations: r.Cfg.Pipeline.MaxIterations,
		ArtifactPath: artifact, PlanPath: ru.planPath, PlanJSONPath: ru.planJSON,
		Feedback: feedback, BudgetLeft: ru.budget.Left(),
	}
}

func (r *Runner) review(ctx context.Context, ru *run, iter int, artifact string, checks []CheckResult) (artifacts.Review, error) {
	head, err := r.Git.Head(ctx, ru.repo.Worktree)
	if err != nil {
		return artifacts.Review{}, err
	}
	status, err := r.Git.Status(ctx, ru.repo.Worktree)
	if err != nil {
		return artifacts.Review{}, err
	}
	data := r.stageData(ru, iter, artifact, "")
	data.Verify = verifySummary(checks)
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
	name, ok := ru.agents[role]
	if !ok {
		return
	}
	if err := r.Herdr.AgentSendKeys(ctx, name, "ctrl+d"); err != nil {
		r.Log.Warn("could not stop agent", "agent", name, "err", err)
	}
	delete(ru.agents, role)
	r.renameTab(ctx, ru, role, role+" ✓")
}

// cleanup stops the run's agents and closes its workspace (step 12). The git worktree stays:
// it holds the branch that will become the MR. Idempotent.
func (r *Runner) cleanup(ctx context.Context, ru *run) {
	for role := range ru.agents {
		r.stopAgent(ctx, ru, role)
	}
	if ru.workspace == "" {
		return
	}
	if err := r.Herdr.WorkspaceClose(ctx, ru.workspace); err != nil {
		r.Log.Warn("could not close workspace", "workspace", ru.workspace, "err", err)
	}
	ru.workspace = ""
}

func (r *Runner) record(ctx context.Context, ru *run) error {
	res := ru.res
	if ru.dir != "" {
		if err := os.MkdirAll(ru.dir, 0o700); err == nil {
			res.ReportPath = filepath.Join(ru.dir, "report.md")
			if err := os.WriteFile(res.ReportPath, []byte(renderReport(*res, ru.task)), 0o600); err != nil {
				r.Log.Warn("could not write report", "err", err)
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

// RandomNonce returns an unguessable token for check sentinels.
func RandomNonce() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
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
