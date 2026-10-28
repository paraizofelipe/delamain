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
