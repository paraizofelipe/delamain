package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
	"github.com/paraizo/delamain/internal/store"
)

// dedupRunner is a Runner with the review_dedup point in mode and a decider giving answers, plus
// a run whose log lands in the returned buffer.
func dedupRunner(mode config.DecisionMode, threshold float64, answers map[string]decide.Answer) (*Runner, *run, *fakeDecider, *bytes.Buffer) {
	d := &fakeDecider{res: decide.Result{Answers: answers, Model: "typesafe/jev-1.14"}}
	cfg := config.Default()
	cfg.Decisions.Enabled = true
	cfg.Decisions.ReviewDedup = config.DecisionPoint{Mode: mode, Threshold: threshold}
	var log bytes.Buffer
	l := slog.New(slog.NewTextHandler(&log, nil))
	return &Runner{Cfg: cfg, Decider: d, Log: l}, &run{rec: store.Run{ID: 91}, log: l}, d, &log
}

// blocker is a major finding of file and category saying message.
func blocker(file, category, message string) artifacts.Finding {
	return artifacts.Finding{Severity: "major", Category: category, File: file, Line: 7, Evidence: "return a - b", Message: message}
}

// axisReview is a review with findings on one axis.
func axisReview(axis string, fs ...artifacts.Finding) artifacts.Review {
	return artifacts.Review{Axes: []artifacts.Axis{{Axis: axis, Executed: true, Findings: fs}}}
}

func TestNovelBlockersShouldMatchMarkBlockersWhenPointIsOff(t *testing.T) {
	r, ru, d, _ := dedupRunner(config.DecisionOff, 0.85, map[string]decide.Answer{"new_0": {Choice: "prev_0", Probabilities: map[string]float64{"prev_0": 1}}})
	seen := newSeenBlockers()
	for i, tc := range []struct {
		review artifacts.Review
		want   bool
	}{
		{axisReview("spec", blocker("a.go", "faltando", "falta Add")), true},
		{axisReview("spec", blocker("b.go", "faltando", "falta Sub")), true},
		// a.go repeats by key, even reworded: not every blocker is new.
		{axisReview("spec", blocker("a.go", "faltando", "Add continua faltando"), blocker("c.go", "faltando", "falta Mul")), false},
		// A review without blockers raises nothing known.
		{axisReview("spec", artifacts.Finding{Severity: "minor", Category: "julgamento", File: "a.go", Message: "nome"}), true},
		// The same key twice in one review: the second is already seen.
		{axisReview("security", blocker("d.go", "vulnerabilidade", "sql"), blocker("d.go", "vulnerabilidade", "sql de novo")), false},
	} {
		if got := r.novelBlockers(context.Background(), ru, i+1, seen, tc.review); got != tc.want {
			t.Fatalf("review %d: novelBlockers = %v, want %v", i+1, got, tc.want)
		}
	}
	if d.calls() != 0 {
		t.Fatalf("off never consults the decider: %d calls", d.calls())
	}
}

func TestNovelBlockersShouldNotConsultOnFirstReview(t *testing.T) {
	r, ru, d, _ := dedupRunner(config.DecisionOn, 0.85, map[string]decide.Answer{"new_0": {Choice: "prev_0", Probabilities: map[string]float64{"prev_0": 1}}})
	if !r.novelBlockers(context.Background(), ru, 1, newSeenBlockers(), axisReview("spec", blocker("a.go", "faltando", "falta Add"))) {
		t.Fatal("the first review's blockers are all new")
	}
	if d.calls() != 0 {
		t.Fatalf("nothing came before the first review to compare with: %d calls", d.calls())
	}
}

func TestNovelBlockersShouldNotConsultWhenAllBlockersAreKnownByKey(t *testing.T) {
	r, ru, d, _ := dedupRunner(config.DecisionOn, 0.85, map[string]decide.Answer{"new_0": {Choice: "nenhum", Probabilities: map[string]float64{"nenhum": 1}}})
	seen := newSeenBlockers()
	r.novelBlockers(context.Background(), ru, 1, seen, axisReview("spec", blocker("a.go", "faltando", "falta Add")))
	if r.novelBlockers(context.Background(), ru, 2, seen, axisReview("spec", blocker("a.go", "faltando", "Add ainda falta"))) {
		t.Fatal("a blocker known by key is not new")
	}
	if d.calls() != 0 {
		t.Fatalf("the key already answers, no question to ask: %d calls", d.calls())
	}
}

// matchPrev0 answers new_0 as the same problem as prev_0 with probability p.
func matchPrev0(p float64) map[string]decide.Answer {
	return map[string]decide.Answer{"new_0": {Choice: "prev_0", Confidence: p, Probabilities: map[string]float64{"prev_0": p, "nenhum": 1 - p}}}
}

// dedupState decodes the request state the way the decisions API reads it.
func dedupState(t *testing.T, req decide.Request) (previous, fresh []map[string]any) {
	t.Helper()
	b, err := json.Marshal(req.State)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Previous []map[string]any `json:"previous"`
		New      []map[string]any `json:"new"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Previous, out.New
}

func TestNovelBlockersShouldTreatMatchedBlockerAsSeenInOnMode(t *testing.T) {
	r, ru, d, log := dedupRunner(config.DecisionOn, 0.85, matchPrev0(0.9))
	seen := newSeenBlockers()
	r.novelBlockers(context.Background(), ru, 1, seen, axisReview("spec", blocker("calc.go", "faltando", "Add(2,3) devolve -1")))
	// Same problem, another file and category: the key differs, the model sees through it.
	if r.novelBlockers(context.Background(), ru, 2, seen, axisReview("spec", blocker("calc_test.go", "quebrado", "a soma de 2 e 3 não dá 5"))) {
		t.Fatal("a reworded blocker matched in on mode is already seen")
	}
	if d.calls() != 1 {
		t.Fatalf("calls = %d, want 1", d.calls())
	}
	for _, want := range []string{`msg="review blocker matched"`, "run=91", "new=spec|calc_test.go|quebrado", "previous=spec|calc.go|faltando", "p=0.9"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the match log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestNovelBlockersShouldAskOnlyBlockersNewByKeyInOneRequest(t *testing.T) {
	r, ru, d, log := dedupRunner(config.DecisionOn, 0.85, map[string]decide.Answer{
		"new_0": {Choice: "nenhum", Probabilities: map[string]float64{"nenhum": 1}},
		"new_1": {Choice: "nenhum", Probabilities: map[string]float64{"nenhum": 1}},
	})
	seen := newSeenBlockers()
	r.novelBlockers(context.Background(), ru, 1, seen, axisReview("spec", blocker("a.go", "faltando", "falta Add"), blocker("b.go", "faltando", "falta Sub")))
	r.novelBlockers(context.Background(), ru, 2, seen, artifacts.Review{Axes: []artifacts.Axis{
		{Axis: "spec", Executed: true, Findings: []artifacts.Finding{blocker("a.go", "faltando", "Add ainda falta"), blocker("c.go", "faltando", "falta Mul")}},
		{Axis: "security", Executed: true, Findings: []artifacts.Finding{
			{Severity: "minor", Category: "julgamento", File: "e.go", Message: "minor nunca é perguntado"},
			blocker("d.go", "vulnerabilidade", "sql injection"),
		}},
	}})

	if d.calls() != 1 {
		t.Fatalf("one request per review: %d calls", d.calls())
	}
	req := d.requests[0]
	if got := slices.Sorted(maps.Keys(req.Choices)); !slices.Equal(got, []string{"new_0", "new_1"}) {
		t.Fatalf("only the blockers new by key are asked: %v", got)
	}
	previous, fresh := dedupState(t, req)
	if len(previous) != 2 || previous[0]["id"] != "prev_0" || previous[0]["file"] != "a.go" || previous[1]["id"] != "prev_1" || previous[1]["file"] != "b.go" {
		t.Fatalf("previous holds what came before this review, oldest first: %v", previous)
	}
	if len(fresh) != 2 || fresh[0]["id"] != "new_0" || fresh[0]["file"] != "c.go" || fresh[1]["id"] != "new_1" ||
		fresh[1]["file"] != "d.go" || fresh[1]["axis"] != "security" || fresh[1]["category"] != "vulnerabilidade" || fresh[1]["message"] != "sql injection" {
		t.Fatalf("new holds the blockers new by key, in review order: %v", fresh)
	}
	c := req.Choices["new_1"]
	if c.Instructions != "O bloqueante new_1 descreve o mesmo problema que algum bloqueante anterior?" {
		t.Errorf("instructions = %q", c.Instructions)
	}
	if got := slices.Sorted(maps.Keys(c.Options)); !slices.Equal(got, []string{"nenhum", "prev_0", "prev_1"}) {
		t.Fatalf("options = %v, want every previous and nenhum", got)
	}
	if c.Options["prev_1"] != "Mesmo problema que prev_1: spec b.go — falta Sub" {
		t.Errorf("prev_1 option = %q", c.Options["prev_1"])
	}
	for _, want := range []string{"msg=decision", "point=review_dedup", "mode=on", "iteration=2", "new_blockers=2", "choice.new_0=nenhum"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the decision log lacks %q:\n%s", want, log.String())
		}
	}
}

// withReviewDedup turns the decisions on with only the review_dedup point, in mode, and a decider giving answers.
func withReviewDedup(hs *harness, mode config.DecisionMode, threshold float64, answers map[string]decide.Answer) *fakeDecider {
	d := &fakeDecider{res: decide.Result{Answers: answers, Model: "typesafe/jev-1.14"}}
	hs.r.Decider = d
	hs.r.Cfg.Decisions.Enabled = true
	hs.r.Cfg.Decisions.Triage = config.DecisionPoint{Mode: config.DecisionOff}
	hs.r.Cfg.Decisions.Checks = config.DecisionPoint{Mode: config.DecisionOff}
	hs.r.Cfg.Decisions.ReviewDedup = config.DecisionPoint{Mode: mode, Threshold: threshold}
	return d
}

func TestExecuteShouldNotStopAsDivergentWhenRepeatedBlockersAreReworded(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "reviewer" {
			// The same problem pinned on another file every time: new by key, repeated in substance.
			return reply{write: reviewBlocking("security", fmt.Sprintf("layer%d.go", n), "vulnerabilidade")}
		}
		return happy(role, n, text)
	})
	hs.r.Cfg.Pipeline.MaxIterations = 4
	d := withReviewDedup(hs, config.DecisionOn, 0.85, matchPrev0(0.9))
	res := hs.run(t)

	if res.Outcome != OutcomeExhausted || res.Iterations != 4 {
		t.Fatalf("a reworded blocker holds the bar still: %s after %d, want the full 4", res.Outcome, res.Iterations)
	}
	if strings.Contains(res.Reason, "review divergente") || !strings.Contains(res.Reason, "4 iterações sem aprovação") {
		t.Fatalf("reason = %q, want the ordinary exhaustion", res.Reason)
	}
	if d.calls() != 3 {
		t.Fatalf("every review after the first asks once: %d calls", d.calls())
	}
}

// twoReviews runs a first review with calc.go and a second whose only blocker is new by key, and
// returns what the second reported.
func twoReviews(r *Runner, ru *run) bool {
	seen := newSeenBlockers()
	r.novelBlockers(context.Background(), ru, 1, seen, axisReview("spec", blocker("calc.go", "faltando", "Add(2,3) devolve -1")))
	return r.novelBlockers(context.Background(), ru, 2, seen, axisReview("spec", blocker("calc_test.go", "quebrado", "a soma de 2 e 3 não dá 5")))
}

func TestNovelBlockersShouldOnlyLogInShadowMode(t *testing.T) {
	r, ru, d, log := dedupRunner(config.DecisionShadow, 0.85, matchPrev0(0.9))
	if !twoReviews(r, ru) {
		t.Fatal("shadow never changes the outcome: the blocker is still new")
	}
	if d.calls() != 1 {
		t.Fatalf("shadow still asks: %d calls", d.calls())
	}
	for _, want := range []string{"msg=decision", "point=review_dedup", "mode=shadow", "model=typesafe/jev-1.14", "choice.new_0=prev_0", "iteration=2"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the decision log lacks %q:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "review blocker matched") {
		t.Errorf("shadow only logs the decision:\n%s", log.String())
	}
}

func TestNovelBlockersShouldKeepBlockerNewWhenChoiceIsNenhum(t *testing.T) {
	for _, choice := range []string{"nenhum", "prev_9"} { // prev_9 was never sent: it counts as nenhum
		r, ru, _, log := dedupRunner(config.DecisionOn, 0.85, map[string]decide.Answer{
			"new_0": {Choice: choice, Confidence: 0.99, Probabilities: map[string]float64{choice: 0.99}},
		})
		if !twoReviews(r, ru) {
			t.Fatalf("choice %s: the blocker is a different problem, still new", choice)
		}
		if strings.Contains(log.String(), "review blocker matched") {
			t.Errorf("choice %s: nothing matched:\n%s", choice, log.String())
		}
	}
}

func TestNovelBlockersShouldKeepBlockerNewBelowThreshold(t *testing.T) {
	r, ru, _, _ := dedupRunner(config.DecisionOn, 0.85, matchPrev0(0.84))
	if !twoReviews(r, ru) {
		t.Fatal("a match below the threshold is not trusted: the blocker is still new")
	}
}

func TestNovelBlockersShouldMatchAtThreshold(t *testing.T) {
	r, ru, _, _ := dedupRunner(config.DecisionOn, 0.85, matchPrev0(0.85))
	if twoReviews(r, ru) {
		t.Fatal("the threshold itself is a match: the blocker is already seen")
	}
}
