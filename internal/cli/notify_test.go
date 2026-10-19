package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

// fakeNotifier records the toasts and how it was created.
type fakeNotifier struct {
	mu      sync.Mutex
	cfg     config.Notify
	opts    notify.Options
	backend string
	got     []notify.Message
	closed  bool
}

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, m)
	return nil
}

func (n *fakeNotifier) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

func (n *fakeNotifier) Backend() string { return n.backend }

// stubNotifier replaces newNotifier for the test; created, when set, runs on creation.
func stubNotifier(t *testing.T, created func()) *fakeNotifier {
	t.Helper()
	fake := &fakeNotifier{backend: "dbus"}
	prev := newNotifier
	newNotifier = func(cfg config.Notify, o notify.Options) notify.Notifier {
		fake.cfg, fake.opts = cfg, o
		if created != nil {
			created()
		}
		return fake
	}
	t.Cleanup(func() { newNotifier = prev })
	return fake
}

func executeContext(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func TestRunCommandShouldCreateNonInteractiveNotifier(t *testing.T) {
	state := t.TempDir()
	t.Setenv("DELAMAIN_STATE_DIR", state)
	fake := stubNotifier(t, nil)
	task := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(task, []byte("# Add notes\n\nno criteria yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := execute(t, "run", "--config", "/nonexistent.yaml", "--task", task, "--repo", gitRepo(t, ""))
	if code, ok := IsExit(err); !ok || code != 4 {
		t.Fatalf("setup: want the needs_info exit, got %v", err)
	}
	if fake.opts.Interactive || fake.opts.StateDir != state || fake.opts.Log == nil || !fake.cfg.Enabled {
		t.Fatalf("`delamain run` has nobody to click: opts=%+v cfg=%+v", fake.opts, fake.cfg)
	}
	if !fake.closed {
		t.Fatal("the notifier is closed when the command ends")
	}
}

func TestDaemonShouldCreateInteractiveNotifier(t *testing.T) {
	state := t.TempDir()
	t.Setenv("DELAMAIN_STATE_DIR", state)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fake := stubNotifier(t, cancel) // the daemon stops right after wiring its runner
	_, _ = executeContext(ctx, t, "daemon", "--config", "/nonexistent.yaml", "--repo", gitRepo(t, "git@github.com:acme/api.git"))
	if !fake.opts.Interactive || fake.opts.StateDir != state {
		t.Fatalf("only the daemon handles clicks: opts=%+v", fake.opts)
	}
	if !fake.closed {
		t.Fatal("the notifier is closed when the daemon stops")
	}
}

func dayStatus(cfg config.Config, spent, cost float64) budget.Status {
	s := budget.Status{Mode: cfg.Budget.Mode, Day: spent, DayCostUSD: cost, Daily: cfg.Budget.Daily, Level: budget.OK}
	switch {
	case spent >= cfg.Budget.Daily:
		s.Level = budget.Hard
	case spent >= cfg.Budget.SoftRatio*cfg.Budget.Daily:
		s.Level = budget.Soft
	}
	return s
}

func titles(n *fakeNotifier) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, m := range n.got {
		out = append(out, m.Title)
	}
	return out
}

func TestDaemonShouldNotifyEachBudgetLevelOncePerDay(t *testing.T) {
	cfg := config.Default() // 10M tokens a day, soft at 80%
	fake := &fakeNotifier{}
	a := newBudgetAlerts(cfg, fake)
	ctx := context.Background()
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local) }

	a.check(ctx, dayStatus(cfg, 2e6, 8), at(19, 9))
	a.check(ctx, dayStatus(cfg, 8.1e6, 32.4), at(19, 10))
	a.check(ctx, dayStatus(cfg, 9e6, 36), at(19, 11))
	a.check(ctx, dayStatus(cfg, 10e6, 40.02), at(19, 12))
	a.check(ctx, dayStatus(cfg, 10e6, 40.02), at(19, 13))
	want := []string{"💸 Daily budget at 80%", "🛑 Daily budget exhausted"}
	if got := titles(fake); !slices.Equal(got, want) {
		t.Fatalf("one toast per level per day, got %q", got)
	}
	if fake.got[0].Body != "8.1M of 10M tokens · US$ 32.40\nNew runs continue until the hard limit" ||
		fake.got[1].Body != "10M of 10M tokens · US$ 40.02\nDaemon paused until 00:00" {
		t.Fatalf("bodies = %q / %q", fake.got[0].Body, fake.got[1].Body)
	}

	a.check(ctx, dayStatus(cfg, 8.5e6, 34), at(20, 0))
	if got := titles(fake); len(got) != 3 || got[2] != "💸 Daily budget at 80%" {
		t.Fatalf("a new day alerts again, got %q", got)
	}

	usd := config.Default()
	usd.Budget = config.Budget{Mode: config.BudgetUSD, Daily: 10, SoftRatio: 0.8}
	fake = &fakeNotifier{}
	newBudgetAlerts(usd, fake).check(ctx, dayStatus(usd, 8.1, 8.1), at(19, 10))
	if len(fake.got) != 1 || fake.got[0].Body != "US$ 8.10 of US$ 10.00\nNew runs continue until the hard limit" {
		t.Fatalf("usd mode shows dollars: %+v", fake.got)
	}

	off := config.Default()
	off.Notify.Events = []string{"halted"}
	fake = &fakeNotifier{}
	newBudgetAlerts(off, fake).check(ctx, dayStatus(off, 10e6, 40), at(19, 10))
	if len(fake.got) != 0 {
		t.Fatalf("budget filtered out of notify.events: %+v", fake.got)
	}
}
