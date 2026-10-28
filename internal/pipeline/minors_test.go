package pipeline

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// minorReview approves with one non-blocking finding on every review.
func minorReview(role string, n int, text string) reply {
	if role == "reviewer" {
		return reply{write: reviewJSON("minor")}
	}
	return happy(role, n, text)
}

func TestRunShouldRunOneMinorPassWhenApprovedWithNonBlockingFindings(t *testing.T) {
	hs := newHarness(t, minorReview)
	hs.r.Cfg.Pipeline.FixMinors = true
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.MinorPass != "aplicado" {
		t.Fatalf("want approved with the minor pass applied, got %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	impl := hs.h.prompts["implementer"]
	if len(impl) != 2 {
		t.Fatalf("the implementer gets exactly one more turn for the minor pass, got %d", len(impl))
	}
	pass := impl[1]
	for _, want := range []string{"Ajustes não bloqueantes (a iteração já foi aprovada)", "subtrai em vez de somar"} {
		if !strings.Contains(pass, want) {
			t.Errorf("the minor pass prompt must carry %q:\n%s", want, pass)
		}
	}
	if strings.Contains(pass, "Feedback da iteração anterior") {
		t.Errorf("the minor pass is not a rejected iteration:\n%s", pass)
	}
	if n := len(hs.h.prompts["reviewer"]); n != 1 {
		t.Fatalf("the minor pass runs no new review, got %d reviews", n)
	}
}

func TestRunShouldPassPlanContextToMinorPassPrompt(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: planWithCalcRef}
		}
		return minorReview(role, n, text)
	})
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.g.onWorktree = seedCalc
	if res := hs.run(t); res.MinorPass != "aplicado" {
		t.Fatalf("want the minor pass applied, got %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	pass := hs.h.prompts["implementer"][1]
	for _, want := range []string{"## Contexto do plano", "### S1 · calc.go:1-1 — Add subtrai", "func Add(a, b int) int { return a + b }"} {
		if !strings.Contains(pass, want) {
			t.Errorf("the minor pass reads the plan's excerpts as the approved iteration left them, missing %q:\n%s", want, pass)
		}
	}
}

func TestRunShouldSkipMinorPassWhenFixMinorsIsOff(t *testing.T) {
	hs := newHarness(t, minorReview)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.MinorPass != "" {
		t.Fatalf("with fix_minors off the run is delivered as before, got %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	if n := len(hs.h.prompts["implementer"]); n != 1 {
		t.Fatalf("with fix_minors off the implementer runs once, got %d", n)
	}
}

func TestRunShouldSkipMinorPassWhenReviewHasNoNonBlockingFindings(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Cfg.Pipeline.FixMinors = true
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.MinorPass != "" {
		t.Fatalf("a review without non-blocking findings needs no pass, got %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	if n := len(hs.h.prompts["implementer"]); n != 1 {
		t.Fatalf("nothing to fix: the implementer runs once, got %d", n)
	}
}

func TestRunShouldSkipMinorPassAtSoftBudget(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := minorReview(role, n, text)
		if role == "planner" {
			rep.usage = 8500
		}
		return rep
	})
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.r.Cfg.Budget.PerIssue = 10_000
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.MinorPass != "pulado: budget" {
		t.Fatalf("past the soft limit the pass is skipped, got %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	if n := len(hs.h.prompts["implementer"]); n != 1 {
		t.Fatalf("a skipped pass spends no implementer turn, got %d", n)
	}
}

func TestMinorPassShouldNotCountAsIteration(t *testing.T) {
	without := newHarness(t, minorReview)
	base := without.run(t)
	with := newHarness(t, minorReview)
	with.r.Cfg.Pipeline.FixMinors = true
	got := with.run(t)

	if got.MinorPass != "aplicado" {
		t.Fatalf("the pass must have run, minor=%q (%s)", got.MinorPass, got.Reason)
	}
	if got.Iterations != base.Iterations || len(got.History) != len(base.History) || got.History[0].N != base.History[0].N {
		t.Fatalf("the pass is not an iteration: iterations %d/%d history %d/%d", got.Iterations, base.Iterations, len(got.History), len(base.History))
	}
	if a, b := with.finalRun(t, got.RunID).Iteration, without.finalRun(t, base.RunID).Iteration; a != b {
		t.Fatalf("the stored iteration must not move: %d, want %d", a, b)
	}
}

// failSecondTest fails the test gate on its second run: the one after the minor pass.
func failSecondTest(name string, n int) int {
	if name == "test" && n == 2 {
		return 1
	}
	return defaultChecks(name, n)
}

func TestMinorPassShouldResetWorktreeWhenGatesFail(t *testing.T) {
	var approvedHead string
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := minorReview(role, n, text)
		if role == "reviewer" {
			rep.mutate = func(g *fakeGit) { approvedHead = g.head } // the head the review approved
		}
		return rep
	})
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.h.checks = failSecondTest
	f := withForge(hs)
	res := hs.run(t)

	if want := []string{hs.g.wt + " " + approvedHead}; !slices.Equal(hs.g.resets, want) {
		t.Fatalf("the pass must be reset to the approved head on the run's worktree, resets=%v want %v", hs.g.resets, want)
	}
	if res.MinorPass != "descartado: gates test falharam" {
		t.Fatalf("the discard must name the failed gates, minor=%q", res.MinorPass)
	}
	if res.Outcome != OutcomeApproved || len(f.created) != 1 || f.created[0].Draft {
		t.Fatalf("a discarded pass keeps the run approved with a ready PR: %s (%s) %+v", res.Outcome, res.Reason, f.created)
	}
	if len(res.Commits) != 1 {
		t.Fatalf("only the approved commit is delivered, commits=%v", res.Commits)
	}
}

// onMinorPass replaces the implementer's second turn, the minor pass, with pass.
func onMinorPass(pass func(rep reply) reply) behavior {
	return func(role string, n int, text string) reply {
		rep := minorReview(role, n, text)
		if role == "implementer" && n == 2 {
			return pass(rep)
		}
		return rep
	}
}

func minorPassWarnings(res Result) []string {
	var ws []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "passe de ajustes") {
			ws = append(ws, w)
		}
	}
	return ws
}

func TestMinorPassShouldResetAndWarnWhenImplementerFails(t *testing.T) {
	hs := newHarness(t, onMinorPass(func(reply) reply { return reply{err: errors.New("boom")} }))
	hs.r.Cfg.Pipeline.FixMinors = true
	f := withForge(hs)
	res := hs.run(t)

	if want := []string{hs.g.wt + " 00000001"}; !slices.Equal(hs.g.resets, want) {
		t.Fatalf("a failed pass is reset to the approved head, resets=%v want %v", hs.g.resets, want)
	}
	if !strings.HasPrefix(res.MinorPass, "descartado:") || !strings.Contains(res.MinorPass, "boom") {
		t.Fatalf("the discard must carry the error, minor=%q", res.MinorPass)
	}
	if ws := minorPassWarnings(res); len(ws) != 1 || !strings.Contains(ws[0], "boom") {
		t.Fatalf("a failed pass leaves exactly one warning with the error, warnings=%v", res.Warnings)
	}
	if res.Outcome != OutcomeApproved || len(f.created) != 1 || f.created[0].Draft {
		t.Fatalf("a failed pass keeps the run approved with a ready PR: %s (%s) %+v", res.Outcome, res.Reason, f.created)
	}
}

func TestMinorPassShouldResetAndStayApprovedWhenBudgetRunsOut(t *testing.T) {
	hs := newHarness(t, onMinorPass(func(rep reply) reply {
		rep.usage = 20_000
		return rep
	}))
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.r.Cfg.Budget.PerIssue = 10_000
	res := hs.run(t)

	if want := []string{hs.g.wt + " 00000001"}; !slices.Equal(hs.g.resets, want) {
		t.Fatalf("a pass stopped by the budget is reset to the approved head, resets=%v want %v", hs.g.resets, want)
	}
	if !strings.HasPrefix(res.MinorPass, "descartado:") || res.Outcome != OutcomeApproved {
		t.Fatalf("the budget discards the pass, not the approval: %s (%s) minor=%q", res.Outcome, res.Reason, res.MinorPass)
	}
	if len(res.Commits) != 1 {
		t.Fatalf("only the approved commit is delivered, commits=%v", res.Commits)
	}
}

func TestMinorPassShouldHaltWithoutDiscardingWhenStopIsRequested(t *testing.T) {
	var hs *harness
	hs = newHarness(t, onMinorPass(func(rep reply) reply {
		requestStop(t, hs, "#7")
		rep.hang = true
		return rep
	}))
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.r.StopPoll = 5 * time.Millisecond
	hs.r.Cfg.Pipeline.PromptTimeout = 5 * time.Second
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || res.Reason != "parada pelo usuário (delamain stop)" {
		t.Fatalf("a stop during the pass halts the run, got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.g.resets) != 0 || res.MinorPass != "" || len(minorPassWarnings(res)) != 0 {
		t.Fatalf("a stop is not a discard: resets=%v minor=%q warnings=%v", hs.g.resets, res.MinorPass, res.Warnings)
	}
}

func TestMinorPassShouldHaltWhenResetFails(t *testing.T) {
	hs := newHarness(t, minorReview)
	hs.r.Cfg.Pipeline.FixMinors = true
	hs.h.checks = failSecondTest
	hs.g.resetErr = errors.New("index.lock exists")
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "index.lock exists") {
		t.Fatalf("a pass that cannot be discarded must not ship as approved, got %s: %s", res.Outcome, res.Reason)
	}
}

func TestRunShouldShowMinorPassStatusInPRAndReport(t *testing.T) {
	hs := newHarness(t, minorReview)
	hs.r.Cfg.Pipeline.FixMinors = true
	f := withForge(hs)
	res := hs.run(t)

	const line = "Passe de ajustes não bloqueantes: aplicado"
	if len(f.created) != 1 {
		t.Fatalf("the approved run must open a PR, created=%+v", f.created)
	}
	if _, after, ok := strings.Cut(f.created[0].Body, "### Achados não bloqueantes"); !ok || !strings.Contains(after, line) {
		t.Errorf("the PR must show the pass status below the non-blocking findings:\n%s", f.created[0].Body)
	}
	report, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, after, ok := strings.Cut(string(report), "#### Achados não bloqueantes"); !ok || !strings.Contains(after, line) {
		t.Errorf("report.md must show the pass status below the non-blocking findings:\n%s", report)
	}
}

func TestMinorPassShouldHaltWhenTheWorktreeLeftTheRunBranch(t *testing.T) {
	hs := newHarness(t, onMinorPass(func(rep reply) reply {
		rep.mutate = func(g *fakeGit) {
			// The pass commits on the run's branch, then checks another one out: a reset there
			// would leave the pass's commits on the branch that gets pushed.
			fixAdd(g)
			g.branch = "outra"
		}
		return rep
	}))
	hs.r.Cfg.Pipeline.FixMinors = true
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "saiu da branch de trabalho") {
		t.Fatalf("a pass that left the run's branch cannot be discarded and must halt, got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.g.resets) != 0 || res.MinorPass != "" {
		t.Fatalf("no reset may run off the run's branch: resets=%v minor=%q", hs.g.resets, res.MinorPass)
	}
	if len(f.created) == 1 && !f.created[0].Draft {
		t.Fatalf("unverified commits must never ship in a ready PR: %+v", f.created)
	}
}
