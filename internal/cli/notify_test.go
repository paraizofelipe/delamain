package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
