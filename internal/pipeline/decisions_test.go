package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
	"github.com/paraizo/delamain/internal/forge"
)

// withTriage turns the decisions on with the triage point in mode and a decider answering actionable=noul.
func withTriage(hs *harness, mode config.DecisionMode, noul float64) *fakeDecider {
	d := &fakeDecider{res: decide.Result{
		Answers: map[string]decide.Answer{"actionable": {Noul: noul}},
		Model:   "typesafe/jev-1.14",
	}}
	hs.r.Decider = d
	hs.r.Cfg.Decisions.Enabled = true
	hs.r.Cfg.Decisions.Triage = config.DecisionPoint{Mode: mode, Threshold: 0.3}
	return d
}

func TestRunShouldReturnNeedsInfoWhenTriageIsOnAndNoulBelowThreshold(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	d := withTriage(hs, config.DecisionOn, 0.12)
	res := hs.run(t)

	if res.Outcome != OutcomeNeedsInfo || !strings.Contains(res.Reason, "triagem automática") {
		t.Fatalf("a vague issue must not start: %s %q", res.Outcome, res.Reason)
	}
	want := "triagem automática: a issue parece não ter objetivo e critérios de aceite verificáveis (p=0.12); detalhe o escopo e os critérios de aceite"
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], want) {
		t.Fatalf("the issue must get the triage text: %v", f.comments)
	}
	if len(f.relabels) != 1 || !slices.Contains(f.relabels[0][0], hs.r.Cfg.Forge.Labels.NeedsInfo) {
		t.Fatalf("labels = %v", f.relabels)
	}
	if runs, _ := hs.st.UnfinishedRuns(context.Background()); len(runs) != 0 || hs.g.worktreeOps != 0 {
		t.Fatal("a triaged-out issue must not create a run or a worktree")
	}
	if d.calls() != 1 {
		t.Fatalf("the decider is asked once, got %d", d.calls())
	}
	q, ok := d.requests[0].Nouls["actionable"]
	if !ok || q.Instructions == "" || q.True == "" || q.False == "" {
		t.Fatalf("triage asks the actionable noul: %+v", d.requests[0].Nouls)
	}
	if st := d.requests[0].State; st["title"] != hs.task.Title || st["kind"] != hs.task.Kind || st["body"] != hs.task.Body {
		t.Fatalf("triage state = %v", st)
	}
}

func TestRunShouldProceedWhenTriageIsOnAndNoulAtThreshold(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	d := withTriage(hs, config.DecisionOn, 0.3)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || d.calls() != 1 {
		t.Fatalf("a noul at the threshold lets the run go: %s %q calls=%d", res.Outcome, res.Reason, d.calls())
	}
	for _, c := range f.comments {
		if strings.Contains(c, "triagem automática") {
			t.Fatalf("no triage comment expected: %q", c)
		}
	}
}

func TestRunShouldNotConsultDeciderWhenDecisionsDisabled(t *testing.T) {
	hs := newHarness(t, happy)
	d := withTriage(hs, config.DecisionOn, 0)
	hs.r.Cfg.Decisions.Enabled = false
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || d.calls() != 0 {
		t.Fatalf("disabled decisions change nothing: %s %q calls=%d", res.Outcome, res.Reason, d.calls())
	}
}

func TestRunShouldNotConsultDeciderWhenTriageIsOff(t *testing.T) {
	hs := newHarness(t, happy)
	d := withTriage(hs, config.DecisionOff, 0)
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || d.calls() != 0 {
		t.Fatalf("triage off changes nothing: %s %q calls=%d", res.Outcome, res.Reason, d.calls())
	}
}

func TestRunShouldNotConsultDeciderWhenPreflightAlreadyFailed(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	d := withTriage(hs, config.DecisionOn, 0)
	hs.task.Body = "sem critérios"
	res := hs.run(t)

	if res.Outcome != OutcomeNeedsInfo || strings.Contains(res.Reason, "triagem") || d.calls() != 0 {
		t.Fatalf("the preflight answers first: %s %q calls=%d", res.Outcome, res.Reason, d.calls())
	}
	if len(f.comments) != 1 || strings.Contains(f.comments[0], "triagem") {
		t.Fatalf("only the preflight problem is posted: %v", f.comments)
	}
}

func TestRunShouldNotConsultDeciderWhenBlockersAreOpen(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	d := withTriage(hs, config.DecisionOn, 0)
	f.issues = map[int]forge.Issue{3: {Number: 3, Open: true}}
	hs.task.Body += "\n\nDepende de #3"
	res := hs.run(t)

	if res.Outcome != OutcomeNeedsInfo || !strings.Contains(res.Reason, "#3") || strings.Contains(res.Reason, "triagem") || d.calls() != 0 {
		t.Fatalf("an open blocker answers first: %s %q calls=%d", res.Outcome, res.Reason, d.calls())
	}
}

func TestAskShouldTreatNilDeciderAsOff(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.Cfg.Decisions.Enabled = true
	var log bytes.Buffer
	l := slog.New(slog.NewTextHandler(&log, nil))
	p := config.DecisionPoint{Mode: config.DecisionOn, Threshold: 0.3}
	req := decide.Request{Nouls: map[string]decide.Noul{"q": {Instructions: "?", True: "y", False: "n"}}}

	res, ok := hs.r.ask(context.Background(), l, "triage", p, req)
	if ok || res.Answers != nil {
		t.Fatalf("a nil decider answers nothing: ok=%v %+v", ok, res)
	}
	if log.Len() != 0 {
		t.Fatalf("a nil decider logs nothing:\n%s", log.String())
	}
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("a nil decider keeps today's run: %s %q", res.Outcome, res.Reason)
	}
}
