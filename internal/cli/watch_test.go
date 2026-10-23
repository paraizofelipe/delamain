package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/paraizo/delamain/internal/budget"
)

// fakeWatchSource is what the TUI reads; it has no way to change a run.
type fakeWatchSource struct {
	active, done []RunView
	status       map[int64]RunStatus
	today        budget.Status
	daemon       bool
	err          error
	finished     int // the last --limit asked of Runs
}

func (f *fakeWatchSource) Runs(_ context.Context, finished int) ([]RunView, []RunView, error) {
	f.finished = finished
	return f.active, f.done, f.err
}

func (f *fakeWatchSource) Status(_ context.Context, id int64) (RunStatus, error) {
	return f.status[id], f.err
}

func (f *fakeWatchSource) Today(context.Context) (budget.Status, error) { return f.today, f.err }

func (f *fakeWatchSource) DaemonRunning() bool { return f.daemon }

func noColors() styles { return newStyles(lipgloss.NewRenderer(&bytes.Buffer{})) }

func TestWatchShouldRefuseANonTerminalOutput(t *testing.T) {
	out, err := execute(t, "--config", filepath.Join(t.TempDir(), "none.yaml"), "watch")
	want := "delamain watch needs a terminal; use 'delamain ls' or 'delamain status'"
	if err == nil || err.Error() != want {
		t.Fatalf("a buffer is not a terminal: err = %v, want %q\n%s", err, want, out)
	}
}

func TestWatchShouldQuitOnQAndRestoreTheTerminal(t *testing.T) {
	for _, key := range []string{"q", "\x03"} { // ctrl+c
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := runWatch(ctx, strings.NewReader(key), &out, &fakeWatchSource{}, noColors(), watchOptions{limit: 20, interval: time.Hour})
		cancel()
		if err != nil {
			t.Fatalf("%q must end the program cleanly: %v", key, err)
		}
		enter, leave := strings.Index(out.String(), "\x1b[?1049h"), strings.LastIndex(out.String(), "\x1b[?1049l")
		if enter < 0 || leave < enter {
			t.Fatalf("%q: the alternate screen must be entered and then left:\n%q", key, out.String())
		}
	}
}
