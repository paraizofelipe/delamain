package pipeline

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/store"
)

// spending makes the implementer consume n input tokens per turn after committing its work.
func spending(n int64, hang bool) behavior {
	return func(role string, i int, text string) reply {
		rep := happy(role, i, text)
		if role == "implementer" {
			rep.usage, rep.hang = n, hang
		}
		return rep
	}
}

func TestBudgetShouldHaltAndDeliverDraftWhenIssueLimitIsReached(t *testing.T) {
	hs := newHarness(t, spending(5000, false))
	hs.r.Cfg.Budget.PerIssue = 1000
	f := withForge(hs)
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "budget da issue esgotado") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.h.prompts["reviewer"]) != 0 {
		t.Fatal("no stage may start after the budget is exhausted")
	}
	if len(f.created) != 1 || !f.created[0].Draft || !strings.Contains(f.created[0].Body, "budget") {
		t.Fatalf("committed work is delivered as draft with the reason: %+v", f.created)
	}
	if !strings.Contains(res.Budget, "5K de 1K") {
		t.Fatalf("result must carry the spend, got %q", res.Budget)
	}
}

func TestBudgetWatcherShouldInterruptAnAgentThatOverspendsMidTurn(t *testing.T) {
	hs := newHarness(t, spending(5000, true)) // the turn never ends on its own
	hs.r.Cfg.Budget.PerIssue = 1000
	hs.r.BudgetInterval = 5 * time.Millisecond
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "budget da issue esgotado") || !strings.Contains(res.Reason, "interrompido") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if keys := hs.h.keys["implementer-r1"]; len(keys) == 0 || keys[0] != "abort" {
		t.Fatalf("the overspending turn must be aborted, keys=%v", keys)
	}
}

// unwrittenSpending makes the implementer report n tokens over RPC that omp has not written to
// its session file yet, as on a session's first turn.
func unwrittenSpending(n int64, hang bool) behavior {
	return func(role string, i int, text string) reply {
		rep := happy(role, i, text)
		if role == "implementer" {
			rep.unwritten, rep.hang = n, hang
		}
		return rep
	}
}

func TestBudgetWatcherShouldInterruptATurnWhoseSpendIsNotWrittenYet(t *testing.T) {
	hs := newHarness(t, unwrittenSpending(5000, true))
	hs.r.Cfg.Budget.PerIssue = 1000
	hs.r.BudgetInterval = 5 * time.Millisecond
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "interrompido") {
		t.Fatalf("the session file lags behind; live usage must still stop the turn: %s: %s", res.Outcome, res.Reason)
	}
	if keys := hs.h.keys["implementer-r1"]; len(keys) == 0 || keys[0] != "abort" {
		t.Fatalf("the overspending turn must be aborted, keys=%v", keys)
	}
}

func TestBudgetShouldHaltAfterATurnWhoseSpendIsNotWrittenYet(t *testing.T) {
	hs := newHarness(t, unwrittenSpending(5000, false))
	hs.r.Cfg.Budget.PerIssue = 1000
	res := hs.run(t)

	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "budget da issue esgotado") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	if len(hs.h.prompts["reviewer"]) != 0 {
		t.Fatal("no stage may start after the budget is exhausted")
	}
}

func TestBudgetShouldNotCountATurnTwiceWhenItIsBothWrittenAndReportedLive(t *testing.T) {
	hs := newHarness(t, spending(600, false)) // the fake reports written usage live as well
	hs.r.Cfg.Budget.PerIssue = 1000
	if res := hs.run(t); res.Outcome != OutcomeApproved {
		t.Fatalf("600 of 1000 must pass, not be read as 1200: %s %s (%s)", res.Outcome, res.Reason, res.Budget)
	}
}

func TestBudgetShouldWarnAgentsPastTheSoftLimit(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "planner" {
			rep.usage = 8500
		}
		return rep
	})
	hs.r.Cfg.Budget.PerIssue = 10_000
	res := hs.run(t)
	if res.Outcome != OutcomeApproved {
		t.Fatalf("soft limit only warns: %s %s", res.Outcome, res.Reason)
	}
	if p := hs.h.prompts["implementer"][0]; !strings.Contains(p, "budget quase no fim") || !strings.Contains(p, "1.5K de 10K") {
		t.Fatalf("implementer must see the remaining budget and the warning:\n%s", p)
	}
}

func TestBudgetShouldRefuseToStartWhenTheDailyCapIsSpent(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	ctx := context.Background()
	old, err := hs.st.CreateRun(ctx, "/other", "#99", "earlier work", "omp", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.st.RecordUsage(ctx, old.ID, "implementer", "/s.jsonl", store.Usage{Input: 11_000_000}); err != nil {
		t.Fatal(err)
	}
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, "diário") || res.RunID != 0 {
		t.Fatalf("got %s (%s) run=%d", res.Outcome, res.Reason, res.RunID)
	}
	if hs.g.worktreeOps != 0 || len(f.relabels) != 0 {
		t.Fatal("a spent day creates nothing and leaves the issue ready for tomorrow")
	}
}

func TestBudgetShouldBeShownInTheHerdrSidebarAndReport(t *testing.T) {
	hs := newHarness(t, spending(1500, false))
	res := hs.run(t)
	if len(hs.h.metadata) == 0 {
		t.Fatal("workspace metadata must be reported")
	}
	for i := 1; i < len(hs.h.metadata); i++ {
		if hs.h.metadata[i].seq <= hs.h.metadata[i-1].seq {
			t.Fatalf("herdr ignores a seq that does not increase: %d after %d", hs.h.metadata[i].seq, hs.h.metadata[i-1].seq)
		}
	}
	last := hs.h.metadata[len(hs.h.metadata)-1].tokens
	if last["iter"] != "1/3" || !strings.HasPrefix(last["tokens"], "1.5K") {
		t.Fatalf("sidebar tokens = %v", last)
	}
	report, _ := os.ReadFile(res.ReportPath)
	if !strings.Contains(string(report), "| Budget | 1.5K de 2M tokens") {
		t.Fatalf("report must show the spend:\n%s", report)
	}
}
