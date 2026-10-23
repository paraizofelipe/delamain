package cli

import (
	"bytes"
	"context"
	"fmt"
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
	reads        int // reloads that reached the source
}

func (f *fakeWatchSource) Runs(_ context.Context, finished int) ([]RunView, []RunView, error) {
	f.finished = finished
	f.reads++
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
	m := tea.Model(newWatchModel(src, noColors(), watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
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
	case watchTickMsg: // the next reload is up to each test
	default:
		m = send(t, m, msg)
	}
	return m
}

// watchNow is the clock of every test model.
var watchNow = time.Date(2026, 10, 23, 12, 47, 5, 0, time.Local)

func screen(m tea.Model) string { return stripANSI(m.View()) }

// has reports whether a panel of view has a row reading want, spaces collapsed.
func has(view, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(view), " "), "│ "+want+" │")
}

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
	if !has(view, "RUN ISSUE STATE CPLX ITER TITLE") {
		t.Errorf("column header = %q:\n%s", line(view, "TITLE"), view)
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
		if !has(view, want) {
			t.Errorf("row of %s = %q, want %q", issue, line(view, issue), want)
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

func TestWatchShouldShowTheStatusOfTheSelectedRun(t *testing.T) {
	run := active(14, "#55", "implement", "low", "artifacts: accept tests")
	run.Harness, run.Branch, run.Base = "claude", "fix/55-artifacts", "main"
	run.Owner = Process{PID: 51230, Alive: true}
	ended := watchNow.Add(-time.Hour + 131*time.Second)
	run.Agents = []AgentView{
		{Role: "planner", State: "done", StartedAt: watchNow.Add(-time.Hour), EndedAt: &ended, Tokens: 41_000, CostUSD: 0.38},
		{Role: "implementer", State: "running", StartedAt: watchNow.Add(-402 * time.Second), Tokens: 141_000, CostUSD: 1.56},
	}
	bare := active(15, "#54", "plan", "", "tui")
	bare.Iteration = 0
	src := &fakeWatchSource{
		active: []RunView{run, bare},
		status: map[int64]RunStatus{14: {Run: run, Budget: BudgetView{Summary: "182K de 4M tokens · US$ 1.94"}}},
	}
	view := screen(startWatch(t, src, 160, 40))

	for _, want := range []string{
		"RUN 14 · #55 artifacts: accept tests", "KIND issue", "HARNESS claude", "STATE implement", "INTERACTIONS 1/3",
		"COMPLEXITY low", "BRANCH fix/55-artifacts (base main)", "OWNER pid 51230 (alive)", "BUDGET 182K de 4M tokens · US$ 1.94",
		"PR/MR -", "RESULT -", "AGENTS", "planner done 2m11s 41K · 0.38", "implementer running 6m42s 141K · 1.56", "logs: coming soon",
	} {
		if !has(view, want) {
			t.Errorf("status panel misses %q:\n%s", want, view)
		}
	}
	for _, title := range []string{"╭ status · run 14 ", "╭ logs · run 14 "} {
		if !strings.Contains(view, title) {
			t.Errorf("missing panel %q:\n%s", title, view)
		}
	}

	src.active = []RunView{bare}
	view = screen(startWatch(t, src, 160, 40))
	for _, want := range []string{"HARNESS -", "INTERACTIONS -", "COMPLEXITY -", "BRANCH -", "OWNER -", "BUDGET -", "no agents yet"} {
		if !has(view, want) {
			t.Errorf("a run with nothing yet shows %q:\n%s", want, view)
		}
	}
}

func keyPress(k string) tea.KeyMsg {
	switch k {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func TestWatchShouldMoveTheSelectionWithArrowsAndJK(t *testing.T) {
	src := &fakeWatchSource{
		active: []RunView{active(1, "#11", "plan", "", "a"), active(2, "#12", "plan", "", "b")},
		done:   []RunView{finished(4, "#14", "approved: ok", "d"), finished(3, "#13", "halted: x", "c")},
		status: map[int64]RunStatus{},
	}
	for id := range int64(5) {
		src.status[id] = RunStatus{Run: RunView{ID: id}, Budget: BudgetView{Summary: fmt.Sprintf("%dK de 2M tokens · US$ 0.0%d", id, id)}}
	}
	m := startWatch(t, src, 160, 40)
	for _, step := range []struct{ key, issue string }{
		{"up", "#11"}, {"k", "#11"}, // stops at the first run
		{"down", "#12"}, {"j", "#14"}, {"j", "#13"}, {"down", "#13"}, // finished runs follow, then it stops
		{"k", "#14"}, {"g", "#11"}, {"G", "#13"}, {"home", "#11"}, {"end", "#13"}, {"up", "#14"},
	} {
		m = send(t, m, keyPress(step.key))
		view := screen(m)
		if row := line(view, "▸"); !strings.Contains(row, " "+step.issue+" ") {
			t.Fatalf("after %s the selection is %s, got row %q", step.key, step.issue, row)
		}
		id := map[string]int64{"#11": 1, "#12": 2, "#13": 3, "#14": 4}[step.issue]
		if !strings.Contains(view, fmt.Sprintf("╭ status · run %d ", id)) || !has(view, fmt.Sprintf("BUDGET %dK de 2M tokens · US$ 0.0%d", id, id)) {
			t.Fatalf("after %s the status panel follows run %d:\n%s", step.key, id, view)
		}
	}
}

// messages runs cmd and returns what it produced, batches flattened.
func messages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, messages(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// split separates the next tick from the other messages.
func split(t *testing.T, msgs []tea.Msg) (tick tea.Msg, others []tea.Msg) {
	t.Helper()
	for _, msg := range msgs {
		if _, ok := msg.(watchTickMsg); ok {
			tick = msg
			continue
		}
		others = append(others, msg)
	}
	if tick == nil {
		t.Fatalf("the next tick must always be scheduled: %v", msgs)
	}
	return tick, others
}

func TestWatchShouldReloadTheDataOnEachTickAndSkipWhileOneIsRunning(t *testing.T) {
	src := &fakeWatchSource{active: []RunView{active(1, "#11", "plan", "", "a")}}
	m := tea.Model(newWatchModel(src, noColors(), watchOptions{limit: 20, interval: time.Millisecond}))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	tick, first := split(t, messages(m.Init()))
	if src.reads != 1 || len(first) != 1 {
		t.Fatalf("Init reads once and schedules a tick: reads %d, %v", src.reads, first)
	}

	// The first reload has not been delivered yet: this tick must not start another one.
	m, cmd := m.Update(tick)
	tick, others := split(t, messages(cmd))
	if src.reads != 1 || len(others) != 0 {
		t.Fatalf("a tick during a reload is skipped: reads %d, %v", src.reads, others)
	}
	m = send(t, m, first[0])

	src.active = append(src.active, active(2, "#12", "plan", "", "b"))
	if strings.Contains(screen(m), "#12") {
		t.Fatal("nothing is read outside a tick")
	}
	m, cmd = m.Update(tick)
	_, others = split(t, messages(cmd))
	if src.reads != 2 || len(others) != 1 {
		t.Fatalf("a tick reloads in a command: reads %d, %v", src.reads, others)
	}
	m = send(t, m, others[0])
	if view := screen(m); !strings.Contains(view, "ACTIVE · 2") || !strings.Contains(view, "#12") {
		t.Fatalf("the reload is shown:\n%s", view)
	}
}

// reload sends one tick and delivers the reload it starts.
func reload(t *testing.T, m tea.Model) tea.Model {
	t.Helper()
	m, cmd := m.Update(watchTickMsg{})
	_, others := split(t, messages(cmd))
	for _, msg := range others {
		m = send(t, m, msg)
	}
	return m
}

func TestWatchShouldKeepTheSelectionByIDWhenARunFinishes(t *testing.T) {
	src := &fakeWatchSource{active: []RunView{active(1, "#11", "plan", "", "a"), active(2, "#12", "implement", "", "b")}}
	m := send(t, startWatch(t, src, 160, 40), keyPress("j"))

	src.active, src.done = src.active[:1], []RunView{finished(2, "#12", "approved: ok", "b")}
	view := screen(reload(t, m))
	if row := line(view, "▸"); !strings.Contains(row, " #12 ") || !strings.Contains(row, "approved") {
		t.Fatalf("the finished run stays selected, now in FINISHED: %q\n%s", row, view)
	}
	if strings.Index(view, "FINISHED · 1") > strings.Index(view, "▸") {
		t.Fatalf("the selection moved with the run to FINISHED:\n%s", view)
	}
}

func TestWatchShouldMoveTheSelectionWhenTheSelectedRunDisappears(t *testing.T) {
	src := &fakeWatchSource{active: []RunView{active(1, "#11", "plan", "", "a"), active(2, "#12", "plan", "", "b"), active(3, "#13", "plan", "", "c")}}
	m := send(t, startWatch(t, src, 160, 40), keyPress("j"))

	src.active = []RunView{src.active[0], src.active[2]}
	m = reload(t, m)
	if row := line(screen(m), "▸"); !strings.Contains(row, " #13 ") {
		t.Fatalf("the run at the same position is selected: %q", row)
	}
	src.active = src.active[:1]
	m = reload(t, m)
	if row := line(screen(m), "▸"); !strings.Contains(row, " #11 ") {
		t.Fatalf("past the end, the last run is selected: %q", row)
	}
	src.active = nil
	if view := screen(reload(t, m)); !strings.Contains(view, "no runs yet") || strings.Contains(view, "▸") {
		t.Fatalf("without runs nothing is selected:\n%s", view)
	}
}
