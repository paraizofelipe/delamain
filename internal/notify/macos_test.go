package notify

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/config"
)

// fakeExec records the commands the notifier runs, never through a shell.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string // name + args
}

func (f *fakeExec) run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	return nil
}

func (f *fakeExec) last(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("nothing was run")
	}
	return f.calls[len(f.calls)-1]
}

// macSys is a Mac where terminal-notifier is installed when tn is true; osascript always is.
func macSys(x *fakeExec, tn bool) sys {
	return sys{
		goos:   "darwin",
		getenv: env(nil),
		dial:   func(context.Context, string) (bus, error) { return nil, errors.New("no bus on macOS") },
		lookPath: func(name string) (string, error) {
			switch {
			case name == "osascript":
				return "/usr/bin/osascript", nil
			case tn && name == "terminal-notifier":
				return "/opt/homebrew/bin/terminal-notifier", nil
			}
			return "", errors.New("not found")
		},
		run: x.run,
	}
}

func newMacClient(t *testing.T, x *fakeExec, tn bool, opts Options) *Client {
	t.Helper()
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	opts.Log = discardLog()
	c := newWith(config.Notify{Enabled: true, Backend: "macos", Events: allEvents()}, opts, macSys(x, tn))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestMacOSShouldPreferTerminalNotifierWithGroupAndContentImage(t *testing.T) {
	x, state := &fakeExec{}, t.TempDir()
	c := newMacClient(t, x, true, Options{StateDir: state})
	ctx := context.Background()
	m := FormatPROpened(sampleRun())
	if err := c.Notify(ctx, m); err != nil {
		t.Fatal(err)
	}
	logo := filepath.Join(state, "notify", "delamain.png")
	want := []string{"/opt/homebrew/bin/terminal-notifier",
		"-title", "✅ PR opened · delamain #12",
		"-message", "Mostrar HARNESS e INTERACTIONS no status · PR #31 · 2 iterations · 167K tokens · US$ 0.67",
		"-group", "delamain-run-42", "-contentImage", logo}
	if got := x.last(t); !slices.Equal(got, want) {
		t.Fatalf("command =\n%q\nwant\n%q", got, want)
	}

	r := sampleRun()
	r.Reason = "x"
	if err := c.Notify(ctx, FormatHalted(r)); err != nil {
		t.Fatal(err)
	}
	got := x.last(t)
	if i := slices.Index(got, "-sound"); i < 0 || i+1 >= len(got) || got[i+1] != "default" {
		t.Fatalf("a critical toast plays the default sound: %q", got)
	}
}

func TestMacOSShouldPassOpenOnlyWhenInteractive(t *testing.T) {
	ctx := context.Background()
	m := FormatPROpened(sampleRun())

	x := &fakeExec{}
	if err := newMacClient(t, x, true, Options{ForgeHosts: []string{"github.com"}}).Notify(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := x.last(t); slices.Contains(got, "-open") {
		t.Fatalf("outside the daemon there is no click: %q", got)
	}

	x = &fakeExec{}
	if err := newMacClient(t, x, true, Options{Interactive: true, ForgeHosts: []string{"github.com"}}).Notify(ctx, m); err != nil {
		t.Fatal(err)
	}
	got := x.last(t)
	if i := slices.Index(got, "-open"); i < 0 || i+1 >= len(got) || got[i+1] != samplePR {
		t.Fatalf("in the daemon the click opens the PR: %q", got)
	}

	for name, target := range map[string]string{
		"foreign host": "https://evil.example/acme/api/pull/31",
		"plain http":   "http://github.com/acme/api/pull/31",
		"report path":  "/state/runs/delamain/42/report.md",
	} {
		x = &fakeExec{}
		bad := m
		bad.Actions = []Action{{Key: "default", Label: "Open PR", Target: target}}
		if err := newMacClient(t, x, true, Options{Interactive: true, ForgeHosts: []string{"github.com"}}).Notify(ctx, bad); err != nil {
			t.Fatal(err)
		}
		if got := x.last(t); slices.Contains(got, "-open") {
			t.Errorf("%s: only https URLs of a known forge are opened: %q", name, got)
		}
	}
}

func TestOsascriptShouldPassTextAsArgvNeverInsideTheScript(t *testing.T) {
	x := &fakeExec{}
	c := newMacClient(t, x, false, Options{Interactive: true, ForgeHosts: []string{"github.com"}})
	title := `⛔ "x" \ end tell & do shell script "rm -rf ~"`
	m := Message{Event: EventHalted, RunID: 42, Title: title, Body: "line 1\nline 2\nline 3", Urgency: Critical,
		Actions: []Action{{Key: "default", Label: "Open PR", Target: samplePR}}}
	if err := c.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	want := []string{"osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--", title, "line 1 · line 2 · line 3"}
	if got := x.last(t); !slices.Equal(got, want) {
		t.Fatalf("command =\n%q\nwant\n%q", got, want)
	}
}
