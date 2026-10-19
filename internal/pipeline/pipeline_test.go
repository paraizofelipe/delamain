package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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
"commands":{"test":"go test ./...","test_file":"","lint":"go vet ./...","typecheck":"","format_check":""},"risks":[]}`
	implJSON = `{"slices":[{"id":"S1","status":"done","red":{"command":"go test","output":"want 5 got -1"},"green":{"command":"go test","output":"ok"},"commits":["abc"]}],
"feedback_addressed":[],"full_suite":{"command":"go test ./...","passed":1,"failed":0,"skipped":0},"lint":{"command":"go vet","ok":true},"typecheck":{"command":"","ok":true},"pending":[]}`
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
	if rec := hs.finalRun(t, res.RunID); !strings.HasPrefix(rec.Reason, "exhausted") {
		t.Fatalf("store reason = %q", rec.Reason)
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
	if _, err := hs.st.CreateRun(context.Background(), hs.task.Repo, hs.task.Ref, "x", "omp", 3, store.KindIssue); err != nil {
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
