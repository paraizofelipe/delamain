package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
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

// startWatch is the TUI after its first window size and first reload.
func startWatch(t *testing.T, src *fakeWatchSource, width, height int) tea.Model {
	t.Helper()
	m := tea.Model(newWatchModel(src, noColors(), watchOptions{limit: 20, interval: time.Millisecond}))
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	return settle(t, m, m.Init())
}

// send delivers msg and runs whatever the model asks for in return.
func send(t *testing.T, m tea.Model, msg tea.Msg) tea.Model {
	t.Helper()
	m, cmd := m.Update(msg)
	return settle(t, m, cmd)
}

// settle runs cmd, as the program would, until the model asks for nothing else.
func settle(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = settle(t, m, c)
		}
	default:
		m = send(t, m, msg)
	}
	return m
}

func screen(m tea.Model) string { return stripANSI(m.View()) }

// line is the first screen line containing s, or "".
func line(view, s string) string {
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, s) {
			return l
		}
	}
	return ""
}

func active(id int64, issue, state, cplx, title string) RunView {
	return RunView{ID: id, Kind: "issue", Issue: issue, State: state, Complexity: cplx, Title: title, Iteration: 1, iterations: 3, Agents: []AgentView{}}
}

func finished(id int64, issue, result, title string) RunView {
	v := active(id, issue, "recorded", "medium", title)
	v.Result, v.terminal = result, true
	return v
}

func TestWatchShouldListActiveAndFinishedRuns(t *testing.T) {
	src := &fakeWatchSource{
		active: []RunView{active(14, "#55", "implement", "low", "artifacts: accept tests"), active(15, "#54", "plan", "", "tui: delamain watch")},
		done:   []RunView{finished(12, "#49", "approved: merged", "logs: colors"), finished(9, "#43", "halted: budget", "daemon: lock")},
	}
	view := screen(startWatch(t, src, 160, 40))

	if !strings.Contains(view, "╭ runs ") {
		t.Errorf("the list is a rounded panel titled runs:\n%s", view)
	}
	header := strings.Fields(line(view, "TITLE"))
	if strings.Join(header, " ") != "│ RUN ISSUE STATE CPLX ITER TITLE │" {
		t.Errorf("column header = %q:\n%s", header, view)
	}
	order := []string{"ACTIVE · 2", "#55", "#54", "FINISHED · 2", "#49", "#43"}
	at := -1
	for _, s := range order {
		i := strings.Index(view, s)
		if i <= at {
			t.Fatalf("want %q in this order: %q\n%s", s, order, view)
		}
		at = i
	}
	for issue, want := range map[string]string{
		"#55": "▸ 14 #55 implement low 1/3 artifacts: accept tests",
		"#54": "15 #54 plan wait 1/3 tui: delamain watch",
		"#49": "12 #49 approved medium 1/3 logs: colors",
		"#43": "9 #43 halted medium 1/3 daemon: lock",
	} {
		row := strings.Join(strings.Fields(strings.Trim(line(view, issue), "│")), " ")
		if row != want {
			t.Errorf("row of %s = %q, want %q", issue, row, want)
		}
	}
	if src.finished != 20 {
		t.Errorf("the source is asked for --limit finished runs, got %d", src.finished)
	}
}

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

func TestWatchShouldShowAnEmptyStateWithoutRuns(t *testing.T) {
	view := screen(startWatch(t, &fakeWatchSource{}, 160, 40))
	if !strings.Contains(view, "no runs yet: label an issue with delamain:ready") {
		t.Errorf("without runs the list says how to start one:\n%s", view)
	}
	for _, s := range []string{"TITLE", "ACTIVE", "FINISHED", "▸"} {
		if strings.Contains(view, s) {
			t.Errorf("an empty list has no %q:\n%s", s, view)
		}
	}
}

func TestWatchSourceShouldReadRunsFromTheStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.run(t, "#1", "", store.StateImplement)
	f.run(t, "#2", "", store.StateRecorded)
	f.run(t, "#3", "", store.StateNeedsInfo)
	f.run(t, "#4", "", store.StatePlan)
	f.run(t, "#5", "", store.StateRecorded)
	_, _ = f.st.RecordUsage(ctx, first.ID, "implementer", "/s.jsonl", store.Usage{Input: 1500, CostUSD: 0.25})
	in, done, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	src := inspectorWatchSource{in: in}

	ids := func(vs []RunView) (out []string) {
		for _, v := range vs {
			out = append(out, v.Issue)
		}
		return out
	}
	for limit, want := range map[int]string{2: "#5 #3", 5: "#5 #3 #2", 0: ""} {
		active, finished, err := src.Runs(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(ids(active), " "); got != "#1 #4" {
			t.Errorf("limit %d: active runs oldest first, every one of them: %q", limit, got)
		}
		if got := strings.Join(ids(finished), " "); got != want {
			t.Errorf("limit %d counts only finished runs, newest first: got %q, want %q", limit, got, want)
		}
	}

	s, err := src.Status(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Issue != "#1" || s.Budget.Summary != "1.5K de 2M tokens · US$ 0.25" {
		t.Errorf("status of run %d = %+v", first.ID, s)
	}
	today, err := src.Today(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if today.Day != 1500 || today.Daily != 10_000_000 || today.Mode != config.BudgetTokens {
		t.Errorf("today = %+v", today)
	}
}
