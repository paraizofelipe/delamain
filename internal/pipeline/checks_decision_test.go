package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
)

// withChecks turns the decisions on with the checks point in mode and a decider giving answers;
// the triage is off so the decider only counts checks requests.
func withChecks(hs *harness, mode config.DecisionMode, threshold float64, answers map[string]decide.Answer) *fakeDecider {
	d := &fakeDecider{res: decide.Result{Answers: answers, Model: "typesafe/jev-1.14"}}
	hs.r.Decider = d
	hs.r.Cfg.Decisions.Enabled = true
	hs.r.Cfg.Decisions.Triage = config.DecisionPoint{Mode: config.DecisionOff}
	hs.r.Cfg.Decisions.Checks = config.DecisionPoint{Mode: mode, Threshold: threshold}
	return d
}

// cause answers one check_<i> choice with the given flaky and infra probabilities.
func cause(flaky, infra float64) decide.Answer {
	return decide.Answer{Choice: "flaky", Confidence: flaky, Probabilities: map[string]float64{
		"teste_real": 1 - flaky - infra, "lint_format": 0, "flaky": flaky, "infra": infra,
	}}
}

// failOnRuns makes the gate name fail on the given runs and every other gate behave by default.
func failOnRuns(name string, runs ...int) func(string, int) int {
	return func(n string, i int) int {
		if n == name {
			for _, r := range runs {
				if r == i {
					return 1
				}
			}
			return 0
		}
		return defaultChecks(n, i)
	}
}

// checkStateItems decodes state.checks the way the decisions API reads it.
func checkStateItems(t *testing.T, state map[string]any) []map[string]any {
	t.Helper()
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out.Checks
}

func TestExecuteShouldRetryVerifyOnceWhenAllFailuresAreFlakyOrInfra(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1)
	d := withChecks(hs, config.DecisionOn, 0.8, map[string]decide.Answer{"check_0": cause(0.6, 0.3)})
	var log bytes.Buffer
	hs.r.Log = slog.New(slog.NewTextHandler(&log, nil))
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 1 {
		t.Fatalf("a flaky failure that passes on retry must not cost an iteration: %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	if d.calls() != 1 || hs.h.checkRuns["test"] != 2 {
		t.Fatalf("one decision and one retry: calls=%d test runs=%d", d.calls(), hs.h.checkRuns["test"])
	}
	if len(hs.h.prompts["implementer"]) != 1 || len(hs.h.prompts["reviewer"]) != 1 {
		t.Fatalf("the retried iteration goes to review, not back to the implementer: impl=%d review=%d",
			len(hs.h.prompts["implementer"]), len(hs.h.prompts["reviewer"]))
	}
	if !res.History[0].Verified {
		t.Fatalf("the retry result replaces the checks: %+v", res.History[0].Checks)
	}
	req := d.requests[0]
	q, ok := req.Choices["check_0"]
	if !ok || len(req.Choices) != 1 || q.Instructions != "Qual a causa mais provável desta falha do check test?" {
		t.Fatalf("one choice per failed check: %+v", req.Choices)
	}
	for _, opt := range []string{"teste_real", "lint_format", "flaky", "infra"} {
		if q.Options[opt] == "" {
			t.Errorf("the choice lacks option %q: %+v", opt, q.Options)
		}
	}
	items := checkStateItems(t, req.State)
	if len(items) != 1 || items[0]["name"] != "test" || items[0]["command"] != "go test ./..." || items[0]["exit_code"] != float64(1) {
		t.Fatalf("state.checks carries the failed check: %v", items)
	}
	if _, ok := items[0]["tail"]; !ok {
		t.Fatalf("state.checks carries the tail: %v", items)
	}
	for _, want := range []string{"msg=decision", "point=checks", "choice.check_0=flaky", "msg=\"checks retried\"", "iteration=1", "checks=[test]"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the run log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestExecuteShouldNotConsultDeciderWhenChecksPointIsOff(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1)
	d := withChecks(hs, config.DecisionOff, 0.8, map[string]decide.Answer{"check_0": cause(0.6, 0.3)})
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 2 {
		t.Fatalf("off keeps today's flow: %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	if d.calls() != 0 || hs.h.checkRuns["test"] != 2 {
		t.Fatalf("off never asks nor retries: calls=%d test runs=%d", d.calls(), hs.h.checkRuns["test"])
	}
	if second := hs.h.prompts["implementer"][1]; !strings.Contains(second, "Gate `test` (`go test ./...`) falhou com código 1") {
		t.Fatalf("the failing gate must reach the implementer:\n%s", second)
	}
}

func TestExecuteShouldSendFeedbackWhenRetryFailsAgain(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1, 2)
	d := withChecks(hs, config.DecisionOn, 0.8, map[string]decide.Answer{"check_0": cause(0.6, 0.3)})
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 2 {
		t.Fatalf("a retry that fails again goes to the implementer: %s after %d (%s)", res.Outcome, res.Iterations, res.Reason)
	}
	if d.calls() != 1 || hs.h.checkRuns["test"] != 3 {
		t.Fatalf("at most one decision and one retry per iteration: calls=%d test runs=%d", d.calls(), hs.h.checkRuns["test"])
	}
	if res.History[0].Verified {
		t.Fatalf("the failed retry is what the iteration records: %+v", res.History[0].Checks)
	}
	if second := hs.h.prompts["implementer"][1]; !strings.Contains(second, "Gate `test` (`go test ./...`) falhou com código 1") {
		t.Fatalf("the failing gate must reach the implementer:\n%s", second)
	}
	if len(hs.h.prompts["reviewer"]) != 1 {
		t.Fatal("review is skipped while the retried checks fail")
	}
}

func TestExecuteShouldNotRetryWhenAnyFailureIsBelowThreshold(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = func(name string, n int) int {
		if (name == "lint" || name == "test") && n == 1 {
			return 1
		}
		return defaultChecks(name, n)
	}
	d := withChecks(hs, config.DecisionOn, 0.8, map[string]decide.Answer{
		"check_0": cause(0.6, 0.3), // lint: flaky enough
		"check_1": cause(0.4, 0.3), // test: likely a real failure
	})
	res := hs.run(t)

	if res.Iterations != 2 || d.calls() != 1 {
		t.Fatalf("one real failure sends the iteration back: %d iterations, calls=%d", res.Iterations, d.calls())
	}
	if hs.h.checkRuns["test"] != 2 || hs.h.checkRuns["lint"] != 2 {
		t.Fatalf("no retry below the threshold: test=%d lint=%d", hs.h.checkRuns["test"], hs.h.checkRuns["lint"])
	}
	if len(d.requests[0].Choices) != 2 {
		t.Fatalf("one choice per failed check: %+v", d.requests[0].Choices)
	}
}

func TestExecuteShouldRetryWhenFlakyPlusInfraEqualsThreshold(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1)
	d := withChecks(hs, config.DecisionOn, 0.75, map[string]decide.Answer{"check_0": cause(0.5, 0.25)})
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 1 || d.calls() != 1 || hs.h.checkRuns["test"] != 2 {
		t.Fatalf("the threshold itself allows the retry: %s after %d, calls=%d test runs=%d",
			res.Outcome, res.Iterations, d.calls(), hs.h.checkRuns["test"])
	}
}

// flakyOn turns the checks point on with a decider sure that every failure is flaky.
func flakyOn(hs *harness) *fakeDecider {
	return withChecks(hs, config.DecisionOn, 0.8, map[string]decide.Answer{
		"check_0": cause(0.9, 0.1), "check_1": cause(0.9, 0.1), "check_2": cause(0.9, 0.1),
	})
}

func TestExecuteShouldNotConsultDeciderWhenAGitGateFailed(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" && n == 1 {
			return reply{write: implJSON, mutate: func(g *fakeGit) {
				fixAdd(g)
				g.status = "?? scratch.txt"
			}}
		}
		return happy(role, n, text)
	})
	hs.h.checks = failOnRuns("test", 1)
	d := flakyOn(hs)
	res := hs.run(t)

	if c, ok := checkNamed(res.History[0], "worktree-limpa"); !ok || c.Passed() {
		t.Fatalf("worktree-limpa = %+v, want a failure", c)
	}
	if d.calls() != 0 || hs.h.checkRuns["test"] != 2 || res.Iterations != 2 {
		t.Fatalf("a git gate never asks nor retries: calls=%d test runs=%d iterations=%d", d.calls(), hs.h.checkRuns["test"], res.Iterations)
	}
}

func TestExecuteShouldNotConsultDeciderWhenSecretsGateFailed(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.LookPath = func(string) (string, error) { return "/usr/bin/gitleaks", nil }
	hs.h.checks = func(name string, n int) int {
		if (name == "secrets" || name == "test") && n == 1 {
			return 1
		}
		return defaultChecks(name, n)
	}
	d := flakyOn(hs)
	res := hs.run(t)

	if c, ok := checkNamed(res.History[0], "segredos"); !ok || c.Passed() {
		t.Fatalf("segredos = %+v, want a failure", c)
	}
	if d.calls() != 0 || hs.h.checkRuns["test"] != 2 || hs.h.checkRuns["secrets"] != 2 || res.Iterations != 2 {
		t.Fatalf("a secrets failure never asks nor retries: calls=%d test=%d secrets=%d iterations=%d",
			d.calls(), hs.h.checkRuns["test"], hs.h.checkRuns["secrets"], res.Iterations)
	}
}

func TestExecuteShouldNotConsultDeciderWhenRedProofFailed(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = func(name string, n int) int {
		if name == "redproof" && n == 1 {
			return 0 // the new tests pass on base: the red proof fails
		}
		return defaultChecks(name, n)
	}
	d := flakyOn(hs)
	res := hs.run(t)

	if c, ok := checkNamed(res.History[0], "prova-vermelho"); !ok || c.Passed() {
		t.Fatalf("prova-vermelho = %+v, want a failure", c)
	}
	if d.calls() != 0 || hs.h.checkRuns["redproof"] != 2 || res.Iterations != 2 {
		t.Fatalf("a red proof failure never asks nor retries: calls=%d redproof runs=%d iterations=%d",
			d.calls(), hs.h.checkRuns["redproof"], res.Iterations)
	}
}

func TestExecuteShouldOnlyLogCheckCauseInShadowMode(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1)
	d := withChecks(hs, config.DecisionShadow, 0.8, map[string]decide.Answer{"check_0": cause(0.9, 0.1)})
	d.res.CostUSD = 0.0004
	var log bytes.Buffer
	hs.r.Log = slog.New(slog.NewTextHandler(&log, nil))
	res := hs.run(t)

	if res.Iterations != 2 || d.calls() != 1 || hs.h.checkRuns["test"] != 2 {
		t.Fatalf("shadow never retries: %d iterations, calls=%d test runs=%d", res.Iterations, d.calls(), hs.h.checkRuns["test"])
	}
	if second := hs.h.prompts["implementer"][1]; !strings.Contains(second, "Gate `test` (`go test ./...`) falhou com código 1") {
		t.Fatalf("in shadow the failing gate still reaches the implementer:\n%s", second)
	}
	for _, want := range []string{
		"msg=decision", "point=checks", "mode=shadow", "model=typesafe/jev-1.14", "cost_usd=0.0004",
		"choice.check_0=flaky", "iteration=1", "checks=[test]",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the decision log lacks %q:\n%s", want, log.String())
		}
	}
	if strings.Contains(log.String(), "checks retried") {
		t.Errorf("shadow only logs the decision:\n%s", log.String())
	}
}

func TestExecuteShouldFallBackToFeedbackWhenDeciderFails(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = failOnRuns("test", 1)
	d := withChecks(hs, config.DecisionOn, 0.8, nil)
	d.err = errors.New("decide: status 503")
	var log bytes.Buffer
	hs.r.Log = slog.New(slog.NewTextHandler(&log, nil))
	res := hs.run(t)

	if res.Outcome != OutcomeApproved || res.Iterations != 2 || d.calls() != 1 || hs.h.checkRuns["test"] != 2 {
		t.Fatalf("a failed decision keeps today's flow: %s after %d, calls=%d test runs=%d",
			res.Outcome, res.Iterations, d.calls(), hs.h.checkRuns["test"])
	}
	for _, want := range []string{`msg="decision skipped"`, "point=checks", `reason="decide: status 503"`, "checks=[test]"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the skip log lacks %q:\n%s", want, log.String())
		}
	}
}
