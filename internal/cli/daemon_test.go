package cli

import (
	"context"
	"os"
	"path/filepath"
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

func TestRecoverShouldNotifyOnlyWhenRunsWereRecovered(t *testing.T) {
	n := &fakeNotifier{}
	useFakeNotifier(t, n, nil)
	cfgFile, state := isolatedConfig(t, "forge:\n  kind: none\n")

	if _, err := execute(t, "--config", cfgFile, "recover"); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("nothing recovered, nothing to say: %+v", n.sent)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(state, "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st.CreateRun(ctx, filepath.Join(t.TempDir(), "api"), "#7", "Fix add", "omp", 3)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimRun(ctx, rec.ID, os.Getpid(), 999_999); err != nil || !ok {
		t.Fatal("seed a run whose process died", err)
	}
	_ = st.Close()

	out, err := execute(t, "--config", cfgFile, "recover")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(n.sent) != 1 {
		t.Fatalf("one recover toast sums the recovered runs up, got %+v", n.sent)
	}
	if m := n.sent[0]; m.Event != notify.EventRecover || m.Title != "♻️ Recovered 1 run" || m.Body != "api #7 (halted)" {
		t.Fatalf("toast = %+v", m)
	}
}
