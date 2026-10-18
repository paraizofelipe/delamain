package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/herdr"
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
			Cfg: cfg, Herdr: h, Git: g, Store: st, Prompts: set,
			PromptsTarget: prompts.Target{OmpAgentDir: filepath.Join(root, "omp"), RolesDir: filepath.Join(root, "roles")},
			Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
			Nonce:         func() string { return "n0nce" },
			RetryInterval: time.Millisecond,
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
		args := strings.Join(hs.h.started[name], " ")
		if !strings.Contains(args, "--append-system-prompt "+hs.r.PromptsTarget.RolePath(role)) || !strings.Contains(args, "--model openai-codex/gpt-5.6-terra") {
			t.Errorf("%s started with wrong args: %s", role, args)
		}
		if hs.h.startPanes[name] != hs.h.tabPanes[role] {
			t.Errorf("%s must run in its own tab, started in %q", role, hs.h.startPanes[name])
		}
		if k := hs.h.keys[name]; len(k) != 1 || k[0] != "ctrl+d" {
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

func TestRunShouldHaltWhenAgentIsBlocked(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{status: herdr.StatusBlocked}
		}
		return happy(role, n, text)
	})
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "bloqueado") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
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
	if hs.h.worktreeOps != 0 || res.RunID != 0 {
		t.Fatal("needs-info must not create worktrees or runs")
	}
}

func TestRunShouldHaltWhenBranchAlreadyExists(t *testing.T) {
	hs := newHarness(t, happy)
	hs.g.existing["fix/7-fix-add"] = true
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || hs.h.worktreeOps != 0 {
		t.Fatalf("existing work must never be overwritten: %s, worktree ops %d", res.Outcome, hs.h.worktreeOps)
	}
}

func TestRunShouldHaltWhenHandshakeFails(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.handshake = func(role string) bool { return role != "reviewer" }
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "handshake do reviewer") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldWaitInsteadOfResubmittingAStalledPrompt(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: planJSON, err: &herdr.Error{Code: herdr.CodeAgentPromptStalled}}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	if res.Outcome != OutcomeApproved || hs.h.waits != 1 || len(hs.h.prompts["planner"]) != 1 {
		t.Fatalf("stall → one wait, no resubmission: outcome=%s waits=%d prompts=%d", res.Outcome, hs.h.waits, len(hs.h.prompts["planner"]))
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
	if _, err := hs.st.CreateRun(context.Background(), hs.task.Repo, hs.task.Ref, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := hs.r.Run(context.Background(), hs.task); err == nil {
		t.Fatal("an active run for the same task must block a second one")
	}
}

func TestRunShouldRetryAgentStartWhileFreshPaneShellIsStarting(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.busyStarts = 3
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("a slow shell startup must not halt the run: %s %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldHaltWhenHerdrReturnsAWorkspaceWithLiveAgents(t *testing.T) {
	hs := newHarness(t, happy)
	// After a server restart herdr restored an old run's workspace with its agents.
	hs.h.liveAgents = []herdr.Agent{{Name: "planner-r1", WorkspaceID: "w9"}}
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "planner-r1") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.h.started) != 0 {
		t.Fatal("no agent may be started into panes another run owns")
	}
	if len(hs.h.closed) != 0 {
		t.Fatal("a workspace that is not ours must never be closed")
	}
}

func TestRunShouldIgnoreLiveAgentsOfOtherWorkspaces(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.liveAgents = []herdr.Agent{{Name: "reviewer", WorkspaceID: "w1"}}
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("agents elsewhere are irrelevant: %s %s", res.Outcome, res.Reason)
	}
}

func TestAgentsShouldStartOnlyWhenTheirStageBegins(t *testing.T) {
	var hs *harness
	seen := map[string][]string{} // role -> agents alive when its first prompt arrived
	hs = newHarness(t, func(role string, n int, text string) reply {
		if n == 1 {
			for name := range hs.h.started {
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
	if _, ok := hs.h.started["reviewer-r1"]; ok || slices.Contains(hs.h.tabs, "reviewer") {
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

func TestRunShouldHaltWhenAnAgentTabCannotBeOpened(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.tabErr = errors.New("workspace not found")
	if res := hs.run(t); res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "workspace not found") || len(hs.h.started) != 0 {
		t.Fatalf("got %s: %s started=%v", res.Outcome, res.Reason, hs.h.started)
	}
}
