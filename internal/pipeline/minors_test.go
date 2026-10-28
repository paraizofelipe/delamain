package pipeline

import (
	"strings"
	"testing"
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
