package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

// fakeNotifier records the toasts and how the CLI created it.
type fakeNotifier struct {
	mu      sync.Mutex
	cfg     config.Notify
	opts    notify.Options
	created int
	sent    []notify.Message
	closed  bool
}

func (n *fakeNotifier) Notify(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, m)
	return nil
}

func (n *fakeNotifier) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

func (n *fakeNotifier) events() []notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []notify.Event
	for _, m := range n.sent {
		out = append(out, m.Event)
	}
	return out
}

// useFakeNotifier makes every command create n instead of a real notifier; then runs after creation.
func useFakeNotifier(t *testing.T, n *fakeNotifier, then func()) {
	t.Helper()
	prev := newNotifier
	newNotifier = func(cfg config.Notify, opts notify.Options) notify.Notifier {
		n.mu.Lock()
		n.cfg, n.opts = cfg, opts
		n.created++
		n.mu.Unlock()
		if then != nil {
			then()
		}
		return n
	}
	t.Cleanup(func() { newNotifier = prev })
}

// isolatedConfig points delamain at a temp state dir and returns the config file to use.
func isolatedConfig(t *testing.T, yaml string) (cfgFile, state string) {
	t.Helper()
	state = t.TempDir()
	t.Setenv("DELAMAIN_STATE_DIR", state)
	cfgFile = filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgFile, state
}

// onlyGit leaves git as the only program on PATH.
func onlyGit(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestRunCommandShouldCreateNonInteractiveNotifier(t *testing.T) {
	n := &fakeNotifier{}
	useFakeNotifier(t, n, nil)
	cfgFile, state := isolatedConfig(t, "forge:\n  kind: none\n")
	task := filepath.Join(t.TempDir(), "vague.md")
	if err := os.WriteFile(task, []byte("# Vague task\n\nNo criteria here.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := execute(t, "--config", cfgFile, "run", "--task", task, "--repo", t.TempDir())
	if code, ok := IsExit(err); !ok || code != 4 {
		t.Fatalf("a task without criteria ends as needs-info (exit 4), got %v", err)
	}
	if n.created != 1 || n.opts.Interactive || n.opts.StateDir != state || !n.cfg.Enabled {
		t.Fatalf("run creates one non-interactive notifier on the state dir: created=%d opts=%+v cfg=%+v", n.created, n.opts, n.cfg)
	}
	if !slices.Equal(n.events(), []notify.Event{notify.EventNeedsInfo}) || !n.closed {
		t.Fatalf("the runner sends its toasts through it and the command closes it: events=%v closed=%v", n.events(), n.closed)
	}
}

func TestDaemonShouldCreateInteractiveNotifier(t *testing.T) {
	// The deadline only bounds a daemon that never wired a notifier.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := &fakeNotifier{}
	useFakeNotifier(t, n, cancel) // stop the daemon right after it wired the notifier
	cfgFile, state := isolatedConfig(t, "")
	repo := gitRepo(t, "https://github.com/acme/api.git")
	onlyGit(t) // a daemon that polls must not reach the real gh

	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--config", cfgFile, "daemon", "--repo", repo})
	_ = root.ExecuteContext(ctx)

	if n.created != 1 || !n.opts.Interactive || n.opts.StateDir != state || !slices.Equal(n.opts.ForgeHosts, []string{"github.com"}) {
		t.Fatalf("the daemon creates one interactive notifier that may open its forges' URLs: created=%d opts=%+v", n.created, n.opts)
	}
	if !n.closed {
		t.Fatal("the daemon closes its notifier when it stops")
	}
}
