package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/prompts"
	"github.com/paraizo/delamain/internal/store"
	embedded "github.com/paraizo/delamain/prompts"
)

const (
	planJSON = `{"summary":"fix add","kind":"bug","assumptions":[],
"slices":[{"id":"S1","behavior":"Add sums","seam":"calc.Add","files":["calc.go"],"tests":["calc_test.go::TestAdd"]}],
"acceptance":[{"criterion":"Add(2,3)=5","tests":["calc_test.go::TestAdd"],"flow":true}],
"regression_tests":["calc_test.go::TestAdd"],
"commands":{"test":"go test ./...","test_file":"","lint":"go vet ./...","typecheck":"","format_check":""},"complexity":{"components":"single_module","change":"localized","dependencies":[],"tests":["unit"],"rationale":"um arquivo"},"risks":[]}`
	implJSON = `{"slices":[{"id":"S1","status":"done","red":{"command":"go test","output":"want 5 got -1"},"green":{"command":"go test","output":"ok"},"commits":["abc"]}],
"feedback_addressed":[],"full_suite":{"command":"go test ./...","passed":1,"failed":0,"skipped":0},"lint":{"command":"go vet","ok":true},"typecheck":{"command":"","ok":true},"pending":[],"pr_title":"fix: correct the add operation"}`
)

func reviewJSON(severity string) string {
	findings := "[]"
	if severity != "" {
		findings = `[{"severity":"` + severity + `","category":"faltando","file":"calc.go","line":3,"evidence":"return a - b","rule":"critério Add(2,3)=5","message":"subtrai em vez de somar"}]`
	}
	// The agent always claims approval: the orchestrator must recompute it.
	return `{"verdict":"approve","axes":[{"axis":"spec","executed":true,"findings":` + findings + `},
{"axis":"standards","executed":true,"findings":[]},{"axis":"security","executed":true,"findings":[]}]}`
}

// happy implements on every call and approves every review.
func happy(role string, _ int, _ string) reply {
	switch role {
	case "planner":
		return reply{write: planJSON}
	case "implementer":
		return reply{write: implJSON, mutate: fixAdd}
	default:
		return reply{write: reviewJSON("")}
	}
}

// fixAdd is a well-behaved TDD commit: the fix plus the regression test the plan names.
func fixAdd(g *fakeGit) {
	g.commitFiles("fix: correct add operation", map[string]string{
		"calc.go":      "func Add(a, b int) int { return a + b }",
		"calc_test.go": "func TestAdd(t *testing.T) {}",
	})
}

type harness struct {
	r    *Runner
	h    *fakeHerdr
	g    *fakeGit
	st   *store.Store
	task Task
}

func newHarness(t *testing.T, b behavior) *harness {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Paths.StateDir = filepath.Join(root, "state")
	cfg.Paths.WorktreesDir = filepath.Join(root, "worktrees")
	cfg.Pipeline.PromptTimeout = time.Second
	st, err := store.Open(context.Background(), filepath.Join(root, "state", "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := prompts.Load(embedded.FS)
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(root, "sessions")
	_ = os.MkdirAll(sessions, 0o700)
	g := newFakeGit()
	h := newFakeHerdr(sessions, g, b)
	return &harness{
		r: &Runner{
			Cfg: cfg, Display: HerdrDisplay{Herdr: h}, Agents: h, Checks: h, Git: g, Store: st, Prompts: set,
			PromptsTarget: prompts.Target{OmpAgentDir: filepath.Join(root, "omp"), RolesDir: filepath.Join(root, "roles")},
			Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			LookPath:      func(string) (string, error) { return "", errors.New("not installed") },
			SubagentWait:  time.Nanosecond, // the waiting turns are what the tests assert, not the pause
		},
		h: h, g: g, st: st,
		task: Task{Ref: "#7", Title: "Fix add", Body: "Add subtrai.\n\n## Critérios de aceite\n- [ ] Add(2,3)=5", Kind: "bug", Repo: filepath.Join(root, "repo")},
	}
}

func (hs *harness) run(t *testing.T) Result {
	t.Helper()
	res, err := hs.r.Run(context.Background(), hs.task)
	if err != nil {
		t.Fatalf("run must record its outcome instead of failing: %v", err)
	}
	return res
}

func (hs *harness) finalRun(t *testing.T, id int64) store.Run {
	t.Helper()
	r, err := hs.st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunShouldDeliverApprovedWorkInOneIteration(t *testing.T) {
	hs := newHarness(t, happy)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 1 {
		t.Fatalf("want approved in 1 iteration, got %s (%s) after %d", res.Outcome, res.Reason, res.Iterations)
	}
	if res.Branch != "fix/7-fix-add" || !strings.HasSuffix(res.Worktree, filepath.Join("repo", "fix", "7-fix-add")) {
		t.Fatalf("bug work goes to an isolated fix/ worktree: %s %s", res.Branch, res.Worktree)
	}
	if len(res.Commits) != 1 {
		t.Fatalf("commits = %v", res.Commits)
	}
	rec := hs.finalRun(t, res.RunID)
	if rec.State != store.StateRecorded || !strings.HasPrefix(rec.Reason, "approved") {
		t.Fatalf("store must record the outcome: %+v", rec)
	}
	report, err := os.ReadFile(res.ReportPath)
	if err != nil || !strings.Contains(string(report), "**approved**") {
		t.Fatalf("report: %v\n%s", err, report)
	}
	if !strings.Contains(string(report), "| Complexidade | low") {
		t.Fatalf("report must show the plan's complexity:\n%s", report)
	}
	if want := []string{"planner", "implementer", "reviewer"}; !slices.Equal(hs.h.tabs, want) {
		t.Fatalf("one tab per agent, named after it, opened as it starts: got %v want %v", hs.h.tabs, want)
	}
	if hs.h.labels["w9:t1"] != "checks" {
		t.Fatalf("the worktree's root tab runs the gates and must say so, labels=%v", hs.h.labels)
	}
	for _, role := range []string{"planner", "implementer", "reviewer"} {
		name := role + "-r1"
		if o := hs.h.starts[name]; o.SystemPrompt != hs.r.PromptsTarget.RolePath(role) || o.SessionDir != filepath.Join(res.RunDir, "sessions", role) {
			t.Errorf("%s started with wrong options: %+v", role, o)
		}
		if log := hs.h.tails[hs.h.tabPanes[role]]; !strings.HasSuffix(log, "/logs/"+role+".log'") || log != shellQuote(hs.h.starts[name].LogPath) {
			t.Errorf("%s's tab must follow the session log, got %q (session logs to %q)", role, log, hs.h.starts[name].LogPath)
		}
		if k := hs.h.keys[name]; len(k) != 1 || k[0] != "close" {
			t.Errorf("%s must be stopped at the end, keys=%v", role, k)
		}
	}
	if _, err := os.Stat(hs.r.PromptsTarget.RolePath("planner")); err != nil {
		t.Fatal("prompts must be installed before agents use them")
	}
	if len(hs.h.closed) != 1 || hs.h.closed[0] != "w9" {
		t.Fatalf("the run's workspace must be closed at the end, closed=%v", hs.h.closed)
	}
}

func TestRunShouldRecordTheHarnessAndMaxIterationsOnTheRun(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Cfg.Agent.Harness = config.HarnessClaude
	hs.r.Cfg.Pipeline.MaxIterations = 5
	res := hs.run(t)

	if rec := hs.finalRun(t, res.RunID); rec.Harness != "claude" || rec.MaxIterations != 5 {
		t.Fatalf("the run must keep the harness and max iterations it started with: harness=%q max=%d", rec.Harness, rec.MaxIterations)
	}
}

func TestRunShouldStartEachRoleWithTheModelConfiguredForIt(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Cfg.Agents = map[string]config.AgentSettings{"implementer": {Model: "cheap-fast", Thinking: "low", Effort: "max"}}
	hs.run(t)

	if o := hs.h.starts["implementer-r1"]; o.Model != "cheap-fast" || o.Reasoning != "low" {
		t.Fatalf("implementer must get its own model and the omp thinking: %+v", o)
	}
	for _, name := range []string{"planner-r1", "reviewer-r1"} {
		if o := hs.h.starts[name]; o.Model != "" || o.Reasoning != "" {
			t.Errorf("%s has no entry and must inherit the harness defaults: %+v", name, o)
		}
	}
}

func TestRunShouldFeedFailingCheckOutputIntoNextIteration(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = func(name string, n int) int {
		if name == "test" && n == 1 {
			return 1
		}
		return defaultChecks(name, n)
	}
	res := hs.run(t)
	if res.Outcome != OutcomeApproved || res.Iterations != 2 {
		t.Fatalf("want approved on iteration 2, got %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	second := hs.h.prompts["implementer"][1]
	if !strings.Contains(second, "Gate `test` (`go test ./...`) falhou com código 1") || !strings.Contains(second, "iteração 2 de 3") {
		t.Fatalf("the failing gate must reach the implementer:\n%s", second)
	}
	if len(hs.h.prompts["reviewer"]) != 1 {
		t.Fatal("review is skipped while deterministic checks fail")
	}
}

func TestRunShouldStopAfterMaxIterationsWhenReviewKeepsBlocking(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON("major")} // agent says approve, finding says otherwise
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeExhausted || res.Iterations != 3 {
		t.Fatalf("tolerance is 3 iterations: got %s after %d", res.Outcome, res.Iterations)
	}
	if !strings.Contains(hs.h.prompts["implementer"][1], "subtrai em vez de somar") {
		t.Fatal("blocking review findings must be fed back to the implementer")
	}
	if rec := hs.finalRun(t, res.RunID); !strings.HasPrefix(rec.Reason, "exhausted") || rec.Complexity != "low" {
		t.Fatalf("store reason = %q, complexity = %q", rec.Reason, rec.Complexity)
	}
}

// highPlan is planJSON with two high signals: the planner sized it as high.
var highPlan = strings.Replace(planJSON, `"components":"single_module","change":"localized"`, `"components":"multiple_services","change":"architectural"`, 1)

func TestRunShouldUseTheComplexityProfileForIterations(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		switch role {
		case "planner":
			return reply{write: highPlan}
		case "reviewer":
			return reply{write: reviewJSON("major")}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Complexity.High.MaxIterations = 5
	res := hs.run(t)
	if res.Outcome != OutcomeExhausted || res.Iterations != 5 {
		t.Fatalf("a high plan gets the high profile's 5 iterations: got %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	if !strings.Contains(hs.h.prompts["implementer"][1], "iteração 2 de 5") {
		t.Fatalf("the implementer must be told the profile's tolerance:\n%s", hs.h.prompts["implementer"][1])
	}
	if rec := hs.finalRun(t, res.RunID); rec.MaxIterations != 5 || rec.Complexity != "high" {
		t.Fatalf("store must record the profile: max_iterations=%d complexity=%q", rec.MaxIterations, rec.Complexity)
	}
}

func TestRunShouldHaltBeforeImplementingWhenTheProfileBudgetIsAlreadySpent(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "planner" {
			rep.usage = 500
		}
		return rep
	})
	hs.r.Cfg.Complexity.Low.PerIssue = 1
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "budget da issue esgotado") {
		t.Fatalf("planning already spent the low profile's budget: got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.h.prompts["implementer"]) != 0 {
		t.Fatal("the implementer must not start once the profile's budget is spent")
	}
}

func TestRunShouldRespectConfiguredIterationTolerance(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON("critical")}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Pipeline.MaxIterations = 1
	if res := hs.run(t); res.Outcome != OutcomeExhausted || res.Iterations != 1 {
		t.Fatalf("got %s after %d", res.Outcome, res.Iterations)
	}
}

func TestRunShouldApproveWhenOnlyMinorFindingsRemain(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON("minor")}
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("minor findings do not block: %s", res.Outcome)
	}
}

func TestRunShouldHaltOnProviderErrorEvenWhenHerdrSaysDone(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{provider: "401 Authentication Error"}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "401") {
		t.Fatalf("M0 finding: done + provider error must halt, got %s: %s", res.Outcome, res.Reason)
	}
	if hs.finalRun(t, res.RunID).State != store.StateRecorded {
		t.Fatal("halted runs are still recorded")
	}
}

func TestRunShouldAskOnceToFixAnInvalidArtifact(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" && n == 1 {
			return reply{write: `{"summary":"x"}`}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeApproved {
		t.Fatalf("a corrected artifact lets the run continue: %s %s", res.Outcome, res.Reason)
	}
	if fix := hs.h.prompts["planner"][1]; !strings.Contains(fix, "está ausente ou inválido") {
		t.Fatalf("second planner prompt must be the correction:\n%s", fix)
	}
}

// Claude Code's Agent tool puts a subagent in the background unless the call says otherwise and
// reports its result only in a later turn, so a review turn can end with the axes still running.
// The step must hand over further turns instead of calling the artifact missing: run 14 lost a
// whole review that way, with the subagents still working when the session was closed.
func TestReviewShouldWaitForSubagentsThatOnlyReportInALaterTurn(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role != "reviewer" {
			return happy(role, n, text)
		}
		if n < 3 {
			return reply{} // the axes are still running: no artifact yet
		}
		return reply{write: reviewJSON("")}
	})
	res := hs.run(t)

	if res.Outcome != OutcomeApproved {
		t.Fatalf("a review whose subagents answer late still approves: %s (%s)", res.Outcome, res.Reason)
	}
	if n := len(hs.h.prompts["reviewer"]); n != 3 {
		t.Fatalf("the reviewer gets a turn per wait until it writes: %d prompts", n)
	}
	// The waiting turn asks for patience, never for an invented finding.
	for _, i := range []int{1, 2} {
		w := hs.h.prompts["reviewer"][i]
		if !strings.Contains(w, "ainda não existe") || !strings.Contains(w, "não** invente") {
			t.Fatalf("prompt %d must be the waiting one:\n%s", i, w)
		}
		if strings.Contains(w, "está ausente ou inválido") {
			t.Fatalf("prompt %d must not be the correction yet:\n%s", i, w)
		}
	}
}

// A malformed artifact is not a subagent still running: the step wrote a file, so it gets the
// single correction turn and no waiting.
func TestStepArtifactShouldNotWaitWhenTheArtifactIsMalformed(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: "{"}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)

	if res.Outcome != OutcomeHalted {
		t.Fatalf("got %s (%s)", res.Outcome, res.Reason)
	}
	if n := len(hs.h.prompts["reviewer"]); n != 2 {
		t.Fatalf("exactly one correction turn, no waiting: %d prompts", n)
	}
	if fix := hs.h.prompts["reviewer"][1]; !strings.Contains(fix, "está ausente ou inválido") {
		t.Fatalf("the second prompt is the correction:\n%s", fix)
	}
}

func TestRunShouldHaltWhenArtifactStaysInvalidAfterCorrection(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: "not json"}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || len(hs.h.prompts["planner"]) != 2 {
		t.Fatalf("exactly one correction, then halt: %s after %d prompts", res.Outcome, len(hs.h.prompts["planner"]))
	}
}

func TestRunShouldHaltWhenReviewerChangesTheWorktree(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewJSON(""), mutate: func(g *fakeGit) { g.commit("refactor: reviewer sneaks a fix") }}
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "reviewer alterou") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldHaltWhenImplementerLeavesTheWorkBranch(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{write: implJSON, mutate: func(g *fakeGit) { g.branch = "main"; g.commit("fix: on main") }}
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "saiu da branch") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldTreatUncommittedOrMissingWorkAsFailedGate(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" && n == 1 {
			return reply{write: implJSON, mutate: func(g *fakeGit) { g.status = "?? calc_test.go" }}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeApproved || res.Iterations != 2 {
		t.Fatalf("got %s after %d", res.Outcome, res.Iterations)
	}
	fb := hs.h.prompts["implementer"][1]
	if !strings.Contains(fb, "worktree-limpa") || !strings.Contains(fb, "tem-commits") {
		t.Fatalf("both git gates must fail on iteration 1:\n%s", fb)
	}
}

func TestRunShouldRejectTaskWithoutAcceptanceCriteriaBeforeCreatingAnything(t *testing.T) {
	hs := newHarness(t, happy)
	hs.task.Body = "Só uma frase vaga."
	res := hs.run(t)
	if res.Outcome != OutcomeNeedsInfo || !strings.Contains(res.Reason, "critérios de aceite") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if hs.g.worktreeOps != 0 || res.RunID != 0 {
		t.Fatal("needs-info must not create worktrees or runs")
	}
}

func TestRunShouldHaltWhenBranchAlreadyExists(t *testing.T) {
	hs := newHarness(t, happy)
	hs.g.existing["fix/7-fix-add"] = true
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || hs.g.worktreeOps != 0 {
		t.Fatalf("existing work must never be overwritten: %s, worktree ops %d", res.Outcome, hs.g.worktreeOps)
	}
}

func TestRunShouldHaltWhenAnAgentFailsToStart(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.startErr = failStart("reviewer")
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "o agente reviewer não subiu") || !strings.Contains(res.Reason, "não ficou pronto") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func failStart(role string) func(string) error {
	return func(r string) error {
		if r == role {
			return errors.New("omp não ficou pronto em 1m0s")
		}
		return nil
	}
}

func TestRunShouldHaltWhenATurnTimesOut(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{hang: true}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Pipeline.PromptTimeout = 50 * time.Millisecond
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "não terminou o turno") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldHaltWhenATurnIsAbortedOutsideTheBudget(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{hang: true}
		}
		return happy(role, n, text)
	})
	hs.h.interrupt <- "abort" // e.g. omp itself gave up on the turn
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "o turno do planner foi abortado") || len(hs.h.prompts["planner"]) != 1 {
		t.Fatalf("an aborted turn produced no work and must not pass as done: %s %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldHaltWhenACheckNeverFinishes(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checkHang["test"] = true
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "check test") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldRefuseSecondConcurrentRunForSameTask(t *testing.T) {
	hs := newHarness(t, happy)
	if _, err := hs.st.CreateRun(context.Background(), hs.task.Repo, hs.task.Ref, "x", "omp", 3, 0, store.KindIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.r.Run(context.Background(), hs.task); err == nil {
		t.Fatal("an active run for the same task must block a second one")
	}
}

func TestRunShouldCompleteWithoutADisplay(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Display = nil // the default: herdr is optional
	res := hs.run(t)
	if res.Outcome != OutcomeApproved || len(res.Commits) != 1 {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if hs.h.workspaces+len(hs.h.tabs)+len(hs.h.tails)+len(hs.h.closed)+len(hs.h.metadata) != 0 {
		t.Fatalf("a headless run never touches herdr: tabs=%v tails=%v closed=%v", hs.h.tabs, hs.h.tails, hs.h.closed)
	}
	if res.WorkspaceID != "" || hs.finalRun(t, res.RunID).Workspace != "" {
		t.Fatal("no workspace exists to record")
	}
	if hs.g.worktreeOps != 1 || res.Worktree != filepath.Join(hs.r.Cfg.Paths.WorktreesDir, "repo", res.Branch) {
		t.Fatalf("the worktree is created under worktrees_dir by git itself: ops=%d path=%s", hs.g.worktreeOps, res.Worktree)
	}
}

func TestRunShouldWriteTheOrchestratorLogAndGateOutputUnderTheRunDir(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Display = nil
	res := hs.run(t)
	logs := filepath.Join(res.RunDir, "logs")
	main, err := os.ReadFile(filepath.Join(logs, "delamain.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state run=1 state=plan", "agent started run=1 role=planner pid=4242", "agent stopped run=1 role=reviewer state=done", "state run=1 state=recorded"} {
		if !strings.Contains(string(main), want) {
			t.Errorf("delamain.log must tell the run's story, missing %q:\n%s", want, main)
		}
	}
	if !regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d `).Match(main) {
		t.Errorf("lines carry the same time prefix as agent logs, so they can be interleaved:\n%s", main)
	}
	for _, c := range hs.h.checkCalls {
		if c.Echo != filepath.Join(logs, "checks.log") || c.Dir == "" || c.Timeout != hs.r.Cfg.Pipeline.CheckTimeout {
			t.Fatalf("every gate echoes into the run's checks.log, bounded by check_timeout: %+v", c)
		}
	}
}

func TestRunShouldRecordEachAgentForOtherProcessesToSee(t *testing.T) {
	hs := newHarness(t, happy)
	res := hs.run(t)
	agents, err := hs.st.Agents(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, a := range agents {
		roles = append(roles, a.Role)
		if a.State != store.AgentDone || a.PID != 4242 || !strings.HasSuffix(a.LogPath, "/logs/"+a.Role+".log") || a.EndedAt.IsZero() {
			t.Errorf("%s = %+v", a.Role, a)
		}
	}
	if !slices.Equal(roles, []string{"planner", "implementer", "reviewer"}) {
		t.Fatalf("roles = %v", roles)
	}
}

func TestAgentsShouldStartOnlyWhenTheirStageBegins(t *testing.T) {
	var hs *harness
	seen := map[string][]string{} // role -> agents alive when its first prompt arrived
	hs = newHarness(t, func(role string, n int, text string) reply {
		if n == 1 {
			for name := range hs.h.starts {
				if len(hs.h.keys[name]) == 0 {
					seen[role] = append(seen[role], name)
				}
			}
			slices.Sort(seen[role])
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	want := map[string][]string{
		"planner":     {"planner-r1"},
		"implementer": {"implementer-r1"}, // planner already stopped after a valid plan
		"reviewer":    {"implementer-r1", "reviewer-r1"},
	}
	for role, alive := range want {
		if !slices.Equal(seen[role], alive) {
			t.Errorf("agents alive at the first %s prompt = %v, want %v", role, seen[role], alive)
		}
	}
}

func TestPlannerShouldKeepRunningWhenThePlanNeedsACorrection(t *testing.T) {
	var hs *harness
	hs = newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" && n == 1 {
			return reply{write: "{"}
		}
		if role == "planner" && len(hs.h.keys["planner-r1"]) != 0 {
			t.Error("the planner was stopped before its plan was valid")
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestReviewerShouldNeverStartWhenVerifyNeverPasses(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = func(name string, n int) int {
		if name == "test" {
			return 1
		}
		return defaultChecks(name, n)
	}
	if res := hs.run(t); res.Outcome != OutcomeExhausted {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if _, ok := hs.h.starts["reviewer-r1"]; ok || slices.Contains(hs.h.tabs, "reviewer") {
		t.Fatalf("no review happens, so no reviewer tab or session, tabs=%v", hs.h.tabs)
	}
}

func TestStoppedAgentTabShouldBeMarkedDoneAndKept(t *testing.T) {
	var hs *harness
	hs = newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" && n == 1 && hs.h.labels["w9:t2"] != "planner ✓" {
			t.Errorf("the planner tab must show it finished once the plan is valid, labels=%v", hs.h.labels)
		}
		return happy(role, n, text)
	})
	hs.run(t)
	if hs.h.labels["w9:t2"] != "planner ✓" {
		t.Fatalf("the finished planner's tab stays, so its output can be read, labels=%v", hs.h.labels)
	}
}

func TestRunShouldRecordAnAgentThatDidNotStopCleanlyAsFailed(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.closeErr = errors.New("omp did not exit")
	res := hs.run(t)
	agents, _ := hs.st.Agents(context.Background(), res.RunID)
	if len(agents) == 0 {
		t.Fatal("agents must be recorded")
	}
	for _, a := range agents {
		if a.State != store.AgentFailed {
			t.Errorf("%s = %s", a.Role, a.State)
		}
	}
}

func TestHerdrTabsShouldStartAQuietShellWhenConfigured(t *testing.T) {
	hs := newHarness(t, happy)
	quiet := filepath.Join(t.TempDir(), "herdr-shell")
	hs.r.Display = HerdrDisplay{Herdr: hs.h, QuietShell: quiet}
	hs.run(t)
	if len(hs.h.shellEnv) != 4 {
		t.Fatalf("workspace + 3 agent tabs: %v", hs.h.shellEnv)
	}
	for _, env := range hs.h.shellEnv {
		if !slices.Equal(env, []string{"ZDOTDIR=" + quiet}) {
			t.Fatalf("every tab only runs tail, so it skips the user's zsh startup: %v", hs.h.shellEnv)
		}
	}
	if _, err := os.Stat(filepath.Join(quiet, ".zshrc")); err != nil {
		t.Fatalf("the quiet ZDOTDIR has an empty .zshrc, or zsh's new-user wizard eats the typed command: %v", err)
	}
}

func TestRunShouldContinueWhenTheDisplayFails(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.tabErr = errors.New("workspace not found")
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("the display is never a reason to stop: %s %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldReadTheTurnOutcomeFromTheSessionWhenTheFinalMessageIsLost(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{provider: "529 overloaded", lost: true}
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "529 overloaded") {
		t.Fatalf("a provider error must not pass as done when its frame is lost: %s %s", res.Outcome, res.Reason)
	}
}

// reviewBlocking requests changes with one blocking finding on the given file, so a run can be
// driven through reviews that repeat a blocker or raise a brand-new one each iteration.
func reviewBlocking(axis, file, category string) string {
	finding := `[{"severity":"major","category":"` + category + `","file":"` + file +
		`","line":3,"evidence":"return a - b","rule":"critério Add(2,3)=5","message":"achado em ` + file + `"}]`
	axes := map[string]string{"spec": "[]", "standards": "[]", "security": "[]"}
	axes[axis] = finding
	return `{"verdict":"approve","axes":[{"axis":"spec","executed":true,"findings":` + axes["spec"] +
		`},{"axis":"standards","executed":true,"findings":` + axes["standards"] +
		`},{"axis":"security","executed":true,"findings":` + axes["security"] + `}]}`
}

func TestPrevReviewDigestShouldListEveryFindingWithItsFingerprint(t *testing.T) {
	review := artifacts.Review{Axes: []artifacts.Axis{
		{Axis: "security", Executed: true, Findings: []artifacts.Finding{
			{Severity: "critical", Category: "vulnerabilidade", File: "a.go", Line: 10, Message: "primeira linha\nsegunda linha"},
		}},
		{Axis: "standards", Executed: true, Findings: []artifacts.Finding{
			{Severity: "minor", Category: "julgamento", File: "b.go", Line: 2, Message: "nome misterioso"},
		}},
	}}
	got := prevReviewDigest(review)

	// Minors belong in the digest too: the next review copies them instead of re-deriving them.
	for _, want := range []string{"critical · security · vulnerabilidade", "a.go:10", "minor · standards · julgamento", "b.go:2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("digest must contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "segunda linha") {
		t.Fatalf("the digest is bounded context, not the full report:\n%s", got)
	}
}

func TestReviewShouldReceiveThePreviousReviewDigest(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" && n == 1 {
			return reply{write: reviewBlocking("standards", "calc.go", "violacao")}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeApproved {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}

	first, second := hs.h.prompts["reviewer"][0], hs.h.prompts["reviewer"][1]
	if strings.Contains(first, "<prev_review>") {
		t.Fatalf("the first review has no previous findings to compare against:\n%s", first)
	}
	for _, want := range []string{"<prev_review>", "major · standards · violacao", "calc.go:3", "achado em calc.go"} {
		if !strings.Contains(second, want) {
			t.Fatalf("the second review must carry the previous findings (%q):\n%s", want, second)
		}
	}
	if !strings.Contains(second, "não podia tê-lo visto") {
		t.Fatalf("the ratchet rule must travel with the digest:\n%s", second)
	}
}

func TestExecuteShouldDeliverADraftWhenReviewsKeepRaisingOnlyNewBlockers(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			// A different file every time: the reviewer never repeats itself, so the
			// implementer never learns when it is done. This is run 12's failure mode.
			return reply{write: reviewBlocking("security", fmt.Sprintf("layer%d.go", n), "vulnerabilidade")}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Pipeline.MaxIterations = 6
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeExhausted {
		t.Fatalf("a diverging review must hand the work over, got %s", res.Outcome)
	}
	if res.Iterations != divergingReviews {
		t.Fatalf("the run must stop at %d reviews, not burn all 6: stopped at %d", divergingReviews, res.Iterations)
	}
	if !strings.Contains(res.Reason, "review divergente") {
		t.Fatalf("the reason must say why it stopped, not blame the iteration cap: %q", res.Reason)
	}
	// The work so far is still worth a draft for a human to judge.
	if len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("created = %+v, want one draft PR", f.created)
	}
	if !strings.Contains(f.created[0].Body, "review divergente") {
		t.Fatalf("the draft body must tell the human why it stopped:\n%s", f.created[0].Body)
	}
}

func TestExecuteShouldKeepIteratingWhenABlockerRepeats(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			return reply{write: reviewBlocking("spec", "calc.go", "faltando")}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Pipeline.MaxIterations = 4
	res := hs.run(t)

	// A repeated blocker means the bar is holding still: the run gets its full budget.
	if res.Iterations != 4 {
		t.Fatalf("iterations = %d, want the full 4", res.Iterations)
	}
	if !strings.Contains(res.Reason, "4 iterações sem aprovação") {
		t.Fatalf("reason = %q, want the ordinary exhaustion", res.Reason)
	}
}

func TestApplyComplexityShouldWarnWhenThePlanTouchesTooManyFiles(t *testing.T) {
	var files []string
	for i := range plannedFilesWarning + 1 {
		files = append(files, fmt.Sprintf("pkg/f%d.go", i))
	}
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: strings.Replace(planJSON, `"files":["calc.go"]`, `"files":`+string(raw), 1)}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)

	var warned string
	for _, w := range res.Warnings {
		if strings.Contains(w, "arquivos de produção") {
			warned = w
		}
	}
	if warned == "" {
		t.Fatalf("an oversized phase must be flagged to the human: %v", res.Warnings)
	}
	if !strings.Contains(warned, "dividir") {
		t.Fatalf("the warning must say what to do about it: %q", warned)
	}
}

// requestStop asks the active run of ref to stop, as `delamain stop` does from another process.
func requestStop(t *testing.T, hs *harness, ref string) {
	t.Helper()
	ctx := context.Background()
	rec, err := hs.st.ActiveRun(ctx, hs.task.Repo, ref)
	if err != nil {
		t.Error(err)
		return
	}
	if err := hs.st.RequestStop(ctx, rec.ID); err != nil {
		t.Error(err)
	}
}

func TestRunShouldHaltWhenStopIsRequested(t *testing.T) {
	var hs *harness
	hs = newHarness(t, func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "implementer" {
			// The implementer commits, then is asked to stop in the middle of its turn.
			requestStop(t, hs, "#7")
			rep.hang = true
		}
		return rep
	})
	hs.r.StopPoll = 5 * time.Millisecond
	hs.r.Cfg.Pipeline.PromptTimeout = 5 * time.Second
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || res.Reason != "parada pelo usuário (delamain stop)" {
		t.Fatalf("a stopped run halts with the stop as its reason, got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.h.prompts["reviewer"]) != 0 {
		t.Fatal("no stage may start after the run is stopped")
	}
	if len(f.created) != 1 || !f.created[0].Draft {
		t.Fatalf("what was committed is delivered as a draft: %+v", f.created)
	}
	last := f.relabels[len(f.relabels)-1]
	if !slices.Equal(last[0], []string{hs.r.Cfg.Forge.Labels.NeedsHuman}) {
		t.Fatalf("a stopped issue goes to a human, labels=%v", last)
	}
	report, err := os.ReadFile(res.ReportPath)
	if err != nil || !strings.Contains(string(report), "parada pelo usuário (delamain stop)") {
		t.Fatalf("the report must say the run was stopped: %v\n%s", err, report)
	}
	if rec := hs.finalRun(t, res.RunID); rec.State != store.StateRecorded || rec.Reason != "halted: parada pelo usuário (delamain stop)" {
		t.Fatalf("store must record the stop: %+v", rec)
	}
}

func TestStopShouldOnlyCancelTheRequestedRun(t *testing.T) {
	aHanging, bInFlight, releaseB := make(chan struct{}), make(chan struct{}), make(chan struct{})
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "planner" && n == 1 { // run #7 plans first and keeps planning
			close(aHanging)
			return reply{hang: true}
		}
		if role == "planner" && n == 2 { // run #8 is planning while #7 is stopped
			close(bInFlight)
			rep.release = releaseB
		}
		return rep
	})
	hs.r.StopPoll = 5 * time.Millisecond
	hs.r.Cfg.Pipeline.PromptTimeout = 5 * time.Second
	a, b := hs.task, hs.task
	b.Ref, b.Title = "#8", "Fix sub"

	type outcome struct {
		res Result
		err error
	}
	aDone, bDone := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		res, err := hs.r.Run(context.Background(), a)
		aDone <- outcome{res, err}
	}()
	<-aHanging
	go func() {
		res, err := hs.r.Run(context.Background(), b)
		bDone <- outcome{res, err}
	}()
	<-bInFlight
	requestStop(t, hs, "#7")

	ao := <-aDone
	if ao.err != nil || ao.res.Outcome != OutcomeHalted || ao.res.Reason != "parada pelo usuário (delamain stop)" {
		t.Fatalf("the requested run must halt with the stop reason, got %s: %s (%v)", ao.res.Outcome, ao.res.Reason, ao.err)
	}
	close(releaseB)
	bo := <-bDone
	if bo.err != nil || bo.res.Outcome != OutcomeApproved {
		t.Fatalf("the other run must go on to the end, got %s: %s (%v)", bo.res.Outcome, bo.res.Reason, bo.err)
	}
	if rec := hs.finalRun(t, bo.res.RunID); !strings.HasPrefix(rec.Reason, "approved") {
		t.Fatalf("the other run is recorded as approved: %+v", rec)
	}
}

func TestRunShouldCallCreatedOnceTheRunIsRecorded(t *testing.T) {
	hs := newHarness(t, happy)
	var got []store.Run
	hs.r.Created = func(ctx context.Context, rec store.Run) {
		if _, err := hs.st.GetRun(ctx, rec.ID); err != nil {
			t.Errorf("Created comes after the run is in the store: %v", err)
		}
		got = append(got, rec)
	}
	res := hs.run(t)
	if len(got) != 1 || got[0].ID != res.RunID || got[0].IssueRef != "#7" {
		t.Fatalf("Created is called once with the recorded run: %+v (run %d)", got, res.RunID)
	}

	hs = newHarness(t, happy)
	hs.task.Body = "Só uma frase vaga."
	hs.r.Created = func(context.Context, store.Run) { t.Error("a task the preflight rejects records no run") }
	if res := hs.run(t); res.Outcome != OutcomeNeedsInfo {
		t.Fatalf("got %s", res.Outcome)
	}
}

// planWithCalcRef is planJSON with slice S1 pointing to the first line of calc.go.
var planWithCalcRef = strings.Replace(planJSON, `"tests":["calc_test.go::TestAdd"]}]`,
	`"tests":["calc_test.go::TestAdd"],"refs":[{"path":"calc.go","lines":"1-1","why":"Add subtrai"}]}]`, 1)

// refPlanned plans with planWithCalcRef and behaves as happy otherwise.
func refPlanned(role string, n int, text string) reply {
	if role == "planner" {
		return reply{write: planWithCalcRef}
	}
	return happy(role, n, text)
}

// seedCalc puts the buggy calc.go in the base checkout the refs point to.
func seedCalc(path string) {
	_ = os.WriteFile(filepath.Join(path, "calc.go"), []byte("func Add(a, b int) int { return a - b }\n"), 0o600)
}

func TestRunShouldPassPlanContextToImplementPrompt(t *testing.T) {
	if planWithCalcRef == planJSON {
		t.Fatal("fixture replacement did not apply")
	}
	hs := newHarness(t, refPlanned)
	hs.g.onWorktree = seedCalc
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("want approved, got %s (%s)", res.Outcome, res.Reason)
	}
	impl := hs.h.prompts["implementer"][0]
	for _, want := range []string{"## Contexto do plano", "### S1 · calc.go:1-1 — Add subtrai", "func Add(a, b int) int { return a - b }"} {
		if !strings.Contains(impl, want) {
			t.Errorf("the implementer must get the plan's excerpts, missing %q:\n%s", want, impl)
		}
	}
}

func TestRunShouldNotPassPlanContextToReviewPrompt(t *testing.T) {
	hs := newHarness(t, refPlanned)
	hs.g.onWorktree = seedCalc
	hs.run(t)
	for _, role := range []string{"planner", "reviewer"} {
		for _, p := range hs.h.prompts[role] {
			if strings.Contains(p, "## Contexto do plano") {
				t.Errorf("only the implementer gets the plan's excerpts, %s got them:\n%s", role, p)
			}
		}
	}
	if len(hs.h.prompts["reviewer"]) == 0 {
		t.Fatal("the run must reach the review for this test to mean anything")
	}
}

func TestRunShouldRefreshPlanContextOnEachImplementIteration(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" && n == 1 {
			return reply{write: reviewJSON("major")}
		}
		return refPlanned(role, n, text)
	})
	hs.g.onWorktree = seedCalc
	if res := hs.run(t); res.Outcome != OutcomeApproved || res.Iterations != 2 {
		t.Fatalf("want approved on iteration 2, got %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	first, second := contextSection(hs.h.prompts["implementer"][0]), contextSection(hs.h.prompts["implementer"][1])
	if !strings.Contains(first, "return a - b") {
		t.Fatalf("the first iteration sees the seeded code:\n%s", first)
	}
	if !strings.Contains(second, "func Add(a, b int) int { return a + b }") || strings.Contains(second, "return a - b") {
		t.Fatalf("the second iteration sees the code the first one wrote:\n%s", second)
	}
}

// contextSection is the "## Contexto do plano" section of an implementer prompt, up to the next one.
func contextSection(prompt string) string {
	_, rest, _ := strings.Cut(prompt, "## Contexto do plano")
	section, _, _ := strings.Cut(rest, "\n## ")
	return section
}
