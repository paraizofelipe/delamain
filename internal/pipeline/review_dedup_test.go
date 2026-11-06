package pipeline

import (
	"bytes"
	"context"
	"log/slog"
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
