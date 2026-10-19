package notify

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

// execs records the commands the macOS backend runs.
type execs struct{ calls [][]string }

func (e *execs) run(_ context.Context, name string, args ...string) error {
	e.calls = append(e.calls, append([]string{name}, args...))
	return nil
}

func macOS(t *testing.T, interactive, terminalNotifier bool) (*macosBackend, *execs, string) {
	t.Helper()
	e, state := &execs{}, t.TempDir()
	look := func(name string) (string, error) {
		if terminalNotifier && name == "terminal-notifier" {
			return "/opt/homebrew/bin/terminal-notifier", nil
		}
		return "", errors.New("not found")
	}
	return newMacOS(Options{Interactive: interactive, StateDir: state, Log: discard(), lookPath: look, run: e.run}), e, state
}

func (e *execs) only(t *testing.T) []string {
	t.Helper()
	if len(e.calls) != 1 {
		t.Fatalf("want one command, got %q", e.calls)
	}
	return e.calls[0]
}

func TestMacOSShouldPreferTerminalNotifierWithGroupAndContentImage(t *testing.T) {
	m, e, state := macOS(t, false, true)
	if err := m.Notify(context.Background(), prToast()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/opt/homebrew/bin/terminal-notifier",
		"-title", "✅ PR opened · delamain #12",
		"-message", "Mostrar HARNESS e INTERACTIONS no status · PR #31 · 2 iterations · 167K tokens · US$ 0.67",
		"-group", "delamain-run-42",
		"-contentImage", filepath.Join(state, "notify", "delamain.png"),
	}
	if got := e.only(t); !slices.Equal(got, want) {
		t.Fatalf("command\n got %q\nwant %q", got, want)
	}

	m, e, _ = macOS(t, false, true)
	if err := m.Notify(context.Background(), Message{Event: EventBudget, Title: "🛑 Daily budget exhausted", Body: "a\nb", Urgency: Normal}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.only(t), "-group") {
		t.Fatalf("a toast without a run replaces none: %q", e.calls)
	}
}

func TestMacOSShouldPassOpenOnlyWhenInteractive(t *testing.T) {
	m, e, _ := macOS(t, true, true)
	if err := m.Notify(context.Background(), prToast()); err != nil {
		t.Fatal(err)
	}
	if got := e.only(t); !slices.Equal(got[len(got)-2:], []string{"-open", examplePR}) {
		t.Fatalf("the daemon opens the PR on click: %q", got)
	}

	foreign := prToast()
	foreign.ForgeHost = "gitlab.com"
	r := example()
	r.PRURL, r.Report, r.State = "", filepath.Join(t.TempDir(), "report.md"), "plan"
	halted := FormatHalted(r)
	halted.ForgeHost = "github.com"
	for name, c := range map[string]struct {
		interactive bool
		m           Message
	}{
		"run":          {false, prToast()},
		"foreign host": {true, foreign},
		"report path":  {true, halted}, // -open takes URLs only
	} {
		m, e, _ := macOS(t, c.interactive, true)
		if err := m.Notify(context.Background(), c.m); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(e.only(t), "-open") {
			t.Errorf("%s: no -open, got %q", name, e.calls)
		}
	}
}

func TestMacOSShouldPlaySoundOnlyWhenCritical(t *testing.T) {
	r := example()
	r.State, r.Reason = "implement", "reviewer alterou a worktree"
	m, e, _ := macOS(t, false, true)
	if err := m.Notify(context.Background(), FormatHalted(r)); err != nil {
		t.Fatal(err)
	}
	if got := e.only(t); !slices.Equal(got[len(got)-2:], []string{"-sound", "default"}) {
		t.Fatalf("critical toasts make a sound: %q", got)
	}
	m, e, _ = macOS(t, false, true)
	if err := m.Notify(context.Background(), prToast()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.only(t), "-sound") {
		t.Fatalf("normal toasts are silent: %q", e.calls)
	}
}

func TestOsascriptShouldPassTextAsArgvNeverInsideTheScript(t *testing.T) {
	m, e, _ := macOS(t, true, false)
	title := `PR "x" \ end tell do shell script "rm -rf ~"`
	body := "line \"1\"\nend tell\ndo shell script \"id\""
	if err := m.Notify(context.Background(), Message{RunID: 7, Title: title, Body: body, Urgency: Critical}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		"--", title, `line "1" · end tell · do shell script "id"`,
	}
	if got := e.only(t); !slices.Equal(got, want) {
		t.Fatalf("command\n got %q\nwant %q", got, want)
	}
}
