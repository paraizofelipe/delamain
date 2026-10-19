package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/store"
)

func TestDaemonShouldNotifyEachBudgetLevelOncePerDay(t *testing.T) {
	n := &fakeNotifier{}
	clock := time.Date(2026, 10, 19, 9, 0, 0, 0, time.Local)
	alert := &budgetAlert{notify: n, softRatio: 0.8, now: func() time.Time { return clock }}
	day := func(spent float64, level budget.Level, reason string) budget.Status {
		return budget.Status{Mode: config.BudgetTokens, Day: spent, Daily: 10_000_000, Level: level, Reason: reason,
			DayUsage: store.Usage{Input: int64(spent), CostUSD: spent * 4e-6}}
	}
	var today budget.Status
	exhausted := dayExhausted(func(context.Context) (budget.Status, error) { return today, nil }, alert)
	poll := func(s budget.Status) string {
		t.Helper()
		today = s
		reason, err := exhausted(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return reason
	}

	poll(day(1_000_000, budget.OK, ""))
	poll(day(8_100_000, budget.Soft, "budget diário em 81%"))
	poll(day(9_000_000, budget.Soft, "budget diário em 90%"))
	if reason := poll(day(10_000_000, budget.Hard, "budget diário esgotado")); reason != "budget diário esgotado" {
		t.Fatalf("at the hard limit the daemon stops starting runs, reason=%q", reason)
	}
	poll(day(10_200_000, budget.Hard, "budget diário esgotado"))

	titles := func() []string {
		var out []string
		for _, m := range n.sent {
			out = append(out, m.Title)
		}
		return out
	}
	want := []string{"💸 Daily budget at 80%", "🛑 Daily budget exhausted"}
	if !slices.Equal(titles(), want) {
		t.Fatalf("each level is notified once a day: %q", titles())
	}
	if n.sent[0].Body != "8.1M of 10M tokens · US$ 32.40\nNew runs continue until the hard limit" {
		t.Fatalf("soft toast body = %q", n.sent[0].Body)
	}
	if n.sent[1].Body != "10M of 10M tokens · US$ 40.00\nDaemon paused until 00:00" || n.sent[1].Event != notify.EventBudget {
		t.Fatalf("hard toast = %+v", n.sent[1])
	}

	clock = time.Date(2026, 10, 20, 0, 5, 0, 0, time.Local)
	poll(day(8_000_000, budget.Soft, "budget diário em 80%"))
	if got := titles(); len(got) != 3 || got[2] != "💸 Daily budget at 80%" {
		t.Fatalf("a new day notifies again: %q", got)
	}
}
