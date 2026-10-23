package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

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

// startWatch is the TUI, 40 lines high, after its first window size and first reload.
func startWatch(t *testing.T, src *fakeWatchSource, width int) tea.Model {
	t.Helper()
	m := tea.Model(newWatchModel(src, noColors(), watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: 40})
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
	view := screen(startWatch(t, src, 160))

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
	view := screen(startWatch(t, &fakeWatchSource{}, 160))
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
	view := screen(startWatch(t, src, 160))

	for _, want := range []string{
		"RUN 14 · #55 artifacts: accept tests", "KIND issue", "HARNESS claude", "STATE implement", "INTERACTIONS 1/3",
		"COMPLEXITY low", "BRANCH fix/55-artifacts (base main)", "OWNER pid 51230 (alive)", "BUDGET 182K de 4M tokens · US$ 1.94",
		"PR/MR -", "RESULT -", "AGENTS", "planner done 2m11s 41K · 0.38", "implementer running 6m42s 141K · 1.56",
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
	view = screen(startWatch(t, src, 160))
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
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
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
	m := startWatch(t, src, 160)
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
	m := send(t, startWatch(t, src, 160), keyPress("j"))

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
	m := send(t, startWatch(t, src, 160), keyPress("j"))

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

func TestWatchShouldShowTheDaemonState(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "daemon.lock")
	if daemonRunning(dir) {
		t.Fatal("without daemon.lock no daemon runs")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("watch must not create the daemon lock: %v", err)
	}
	f, err := os.Create(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if daemonRunning(dir) {
		t.Fatal("a lock nobody holds means no daemon")
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		t.Fatal(err)
	}
	for range 2 { // the first probe must leave the lock held
		if !daemonRunning(dir) {
			t.Fatal("a held lock means the daemon runs")
		}
	}

	for running, want := range map[bool]string{true: "daemon ●", false: "daemon ○"} {
		view := screen(startWatch(t, &fakeWatchSource{daemon: running}, 160))
		if head := strings.Split(view, "\n")[0]; !strings.Contains(head, want) {
			t.Errorf("daemon running=%v: header %q, want %q", running, head, want)
		}
	}
}

func TestWatchShouldShowTheDaySpendInTheHeader(t *testing.T) {
	src := &fakeWatchSource{
		active: []RunView{active(1, "#11", "plan", "", "a"), active(2, "#12", "plan", "", "b")},
		done:   []RunView{finished(3, "#13", "approved: ok", "c")},
		today:  budget.Status{Mode: config.BudgetTokens, Day: 1_400_000, Daily: 4_000_000},
	}
	head := strings.Split(screen(startWatch(t, src, 160)), "\n")[0]
	for _, want := range []string{"delamain", "watch · " + Version, "2 active · 1 finished", "today 1.4M of 4M"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q misses %q", head, want)
		}
	}
	if !strings.HasSuffix(head, " 12:47:05") || lipgloss.Width(head) != 160 {
		t.Errorf("the clock is right-aligned on the 160 columns: %q (%d)", head, lipgloss.Width(head))
	}

	for _, c := range []struct {
		today budget.Status
		want  string
	}{
		{budget.Status{Mode: config.BudgetTokens, Day: 1_400_000}, "today 1.4M"}, // no daily limit
		{budget.Status{Mode: config.BudgetUSD, Day: 1.2, Daily: 5}, "today US$ 1.20 of US$ 5.00"},
	} {
		src.today = c.today
		head := strings.Split(screen(startWatch(t, src, 160)), "\n")[0]
		if !strings.Contains(head, c.want+" ") || strings.Contains(head, c.want+" of") {
			t.Errorf("header %q, want %q", head, c.want)
		}
	}

	for level, code := range map[budget.Level]string{budget.OK: "2", budget.Soft: "33", budget.Hard: "31"} {
		src.today = budget.Status{Mode: config.BudgetTokens, Day: 3_500_000, Daily: 4_000_000, Level: level}
		m := tea.Model(newWatchModel(src, lineStyles(termenv.ANSI), watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
		m = settle(t, send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40}), m.Init())
		if want := "\x1b[" + code + "mtoday 3.5M of 4M\x1b[0m"; !strings.Contains(m.View(), want) {
			t.Errorf("level %v colors the day spend %q:\n%q", level, want, strings.Split(m.View(), "\n")[0])
		}
	}
}

func footer(m tea.Model) string {
	lines := strings.Split(screen(m), "\n")
	return lines[len(lines)-1]
}

func TestWatchShouldShowReadErrorsInTheFooterAndKeepRunning(t *testing.T) {
	src := &fakeWatchSource{active: []RunView{active(1, "#11", "plan", "", "a")}}
	m := startWatch(t, src, 160)
	if got := strings.TrimSpace(footer(m)); got != "↑↓ navigate  tab focus  1-6 source  f follow  z full screen  q quit" {
		t.Fatalf("footer = %q", got)
	}

	src.err = errors.New("database is locked")
	m = reload(t, m)
	if got := strings.TrimSpace(footer(m)); got != "error: database is locked" {
		t.Fatalf("a failed read is shown in the footer: %q", got)
	}
	if !strings.Contains(screen(m), "#11") {
		t.Fatalf("a failed read keeps the last data:\n%s", screen(m))
	}

	src.err = nil
	src.active = append(src.active, active(2, "#12", "plan", "", "b"))
	m = reload(t, m)
	if got := strings.TrimSpace(footer(m)); got != "↑↓ navigate  tab focus  1-6 source  f follow  z full screen  q quit" || !strings.Contains(screen(m), "#12") {
		t.Fatalf("the next good read clears the error and keeps reloading: %q\n%s", got, screen(m))
	}
}

// crowded has more runs and longer texts than any window holds.
func crowded() *fakeWatchSource {
	long := strings.Repeat("a very long title that never fits ", 8)
	src := &fakeWatchSource{status: map[int64]RunStatus{}, today: budget.Status{Mode: config.BudgetUSD, Day: 123456.78, Daily: 999999.99}, daemon: true}
	agents := []AgentView{}
	for _, role := range []string{"planner", "implementer", "reviewer", "security-reviewer-with-a-long-name"} {
		agents = append(agents, AgentView{Role: role, State: "running", StartedAt: watchNow.Add(-1000 * time.Hour), Tokens: 123_456_789, CostUSD: 12345.67})
	}
	for id := int64(1); id <= 30; id++ {
		v := active(id, fmt.Sprintf("PR#%d00000", id), "deliver_draft", "medium", long)
		if id > 8 {
			v = finished(id, fmt.Sprintf("#%d00000", id), "exhausted: "+long, long)
		}
		v.Branch, v.Base, v.PR, v.Agents = "feat/"+strings.ReplaceAll(long, " ", "-"), "main", "https://github.com/o/r/pull/"+strings.Repeat("9", 80), agents
		v.Owner = Process{PID: 4_194_304}
		if id <= 8 {
			src.active = append(src.active, v)
		} else {
			src.done = append([]RunView{v}, src.done...)
		}
		src.status[id] = RunStatus{Run: v, Budget: BudgetView{Summary: long}}
	}
	return src
}

func TestWatchShouldNeverRenderWiderOrTallerThanTheWindow(t *testing.T) {
	logs := logRun(t, 1, "#1")
	for i := range 300 {
		writeLog(t, logs, []string{"delamain", "checks", "implementer"}[i%3], fmt.Sprintf("12:%02d:%02d %s\n", i/60, i%60, strings.Repeat("a very long log line that never fits ", 10)))
	}
	for _, width := range []int{30, 40, 56, 70, 80, 84, 99, 120, 139, 160, 179, 200} {
		for _, height := range []int{6, 10, 12, 15, 18, 20, 24, 26, 32, 40, 50} {
			layout := layoutFor(width, height)
			src := crowded()
			for _, vs := range [][]RunView{src.active, src.done} {
				for i := range vs {
					vs[i].RunDir = logs.RunDir
				}
			}
			m := tea.Model(newWatchModel(src, lineStyles(termenv.ANSI), watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
			m = reload(t, settle(t, send(t, m, tea.WindowSizeMsg{Width: width, Height: height}), m.Init()))
			if layout != layoutMini && layout != layoutTooSmall && !strings.Contains(panelText(stripANSI(m.View()), "logs · run 1 "), "fits") { // the end of a log line
				t.Fatalf("%dx%d: the logs are shown:\n%s", width, height, stripANSI(m.View()))
			}
			for _, k := range []string{"", "G", "k"} {
				if k != "" {
					m = send(t, m, keyPress(k))
				}
				lines := strings.Split(m.View(), "\n")
				if len(lines) > height {
					t.Fatalf("%dx%d after %q: %d lines", width, height, k, len(lines))
				}
				for i, l := range lines {
					if w := lipgloss.Width(l); w > width {
						t.Fatalf("%dx%d after %q: line %d is %d wide:\n%s", width, height, k, i, w, stripANSI(l))
					}
				}
				if layout != layoutMini && layout != layoutTooSmall && !strings.Contains(m.View(), "▸") {
					t.Fatalf("%dx%d after %q: the selected run stays visible:\n%s", width, height, k, stripANSI(m.View()))
				}
			}
			for _, k := range []string{"tab", "pgup", "z"} {
				m = send(t, m, keyPress(k))
				lines := strings.Split(m.View(), "\n")
				if len(lines) > height {
					t.Fatalf("%dx%d after %q: %d lines", width, height, k, len(lines))
				}
				for i, l := range lines {
					if w := lipgloss.Width(l); w > width {
						t.Fatalf("%dx%d after %q: line %d is %d wide:\n%s", width, height, k, i, w, stripANSI(l))
					}
				}
			}
			src.err = errors.New(strings.Repeat("database is locked ", 20))
			m = reload(t, m)
			for i, l := range strings.Split(m.View(), "\n") {
				if w := lipgloss.Width(l); w > width {
					t.Fatalf("%dx%d with an error: line %d is %d wide", width, height, i, w)
				}
			}
		}
	}
}

// resize delivers a new window size.
func resize(t *testing.T, m tea.Model, width, height int) tea.Model {
	t.Helper()
	return send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
}

// fits fails the test when a line of view is wider than width or view is taller than height.
func fits(t *testing.T, view string, width, height int) {
	t.Helper()
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		t.Fatalf("%dx%d: %d lines:\n%s", width, height, len(lines), stripANSI(view))
	}
	for i, l := range lines {
		if w := lipgloss.Width(l); w > width {
			t.Fatalf("%dx%d: line %d is %d wide:\n%s", width, height, i, w, stripANSI(view))
		}
	}
}

func TestWatchShouldShowTooSmallBelow40x12(t *testing.T) {
	m := startWatch(t, crowded(), 160)
	for _, size := range [][2]int{{39, 12}, {40, 11}, {30, 10}, {12, 3}} {
		m = resize(t, m, size[0], size[1])
		view := screen(m)
		fits(t, m.View(), size[0], size[1])
		want := fmt.Sprintf("terminal too small: %d×%d (minimum 40×12)", size[0], size[1])
		if got := strings.Join(strings.Fields(view), " "); got != want && size[0] >= 30 {
			t.Fatalf("%dx%d shows only %q, got:\n%s", size[0], size[1], want, view)
		}
		if strings.Contains(view, "╭") || strings.Contains(view, "#") {
			t.Fatalf("%dx%d draws no panel:\n%s", size[0], size[1], view)
		}
	}
	m = resize(t, m, 60, 11)
	lines := strings.Split(screen(m), "\n")
	for i, l := range lines {
		if text := strings.TrimSpace(l); (i == 5) != (text != "") {
			t.Fatalf("the message is alone on the middle line of 11:\n%s", strings.Join(lines, "\n"))
		}
	}
	if left, right := len(lines[5])-len(strings.TrimLeft(lines[5], " ")), 60-lipgloss.Width(strings.TrimRight(lines[5], " ")); left != right && left != right-1 {
		t.Fatalf("the message is centered: %d columns left, %d right: %q", left, right, lines[5])
	}
	if _, cmd := m.Update(keyPress("q")); cmd == nil || cmd() != tea.Quit() {
		t.Fatal("q still quits")
	}
	if view := screen(resize(t, m, 80, 27)); !strings.Contains(view, "╭ runs") {
		t.Fatalf("growing the window brings the panels back:\n%s", view)
	}
}

// panelRows are the rows inside the panel titled title of a screen without colors, nil without it.
func panelRows(view, title string) []string {
	lines := strings.Split(view, "\n")
	for i, l := range lines {
		at := strings.Index(l, "╭ "+title)
		if at < 0 {
			continue
		}
		col := lipgloss.Width(l[:at])
		end := col + lipgloss.Width(l[at:at+strings.Index(l[at:], "╮")])
		var out []string
		for _, row := range lines[i+1:] {
			if lipgloss.Width(row) <= end || ansi.Cut(row, col, col+1) == "╰" {
				break
			}
			out = append(out, ansi.Cut(row, col+2, end-1))
		}
		return out
	}
	return nil
}

// panelText is the inside of the panel titled title, one trimmed line per row; "" without it.
func panelText(view, title string) string {
	rows := panelRows(view, title)
	for i, r := range rows {
		rows[i] = strings.TrimSpace(r)
	}
	return strings.Join(rows, "\n")
}

// words is s with its spaces and line breaks collapsed.
func words(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestWatchCompactShouldStackTheListTheStatusStripAndTheLogs(t *testing.T) {
	v := logRun(t, 14, "#55")
	v.Title = "artifacts: accept test_file"
	writeLog(t, v, "implementer", "12:46:58 💬 adjusted the fixture\n")
	src := &fakeWatchSource{
		active: []RunView{v, active(15, "#54", "plan", "", "tui: delamain watch")},
		done:   []RunView{finished(12, "#49", "approved: merged", "logs: colors")},
		today:  budget.Status{Mode: config.BudgetTokens, Day: 1_400_000, Daily: 4_000_000},
		daemon: true,
	}
	m := startLogs(t, src, 80, false)
	view := screen(m)
	lines := strings.Split(view, "\n")
	if head := lines[0]; !strings.HasPrefix(head, "delamain ● ") || !strings.HasSuffix(head, "2 active · 1 fin.  1.4M/4M") || lipgloss.Width(head) != 80 {
		t.Errorf("compact header = %q", head)
	}
	at := 0
	for _, title := range []string{"╭ runs ", "╭ status · enter details ", "╭ logs · run 14 · ● following "} {
		i := at
		for i < len(lines) && !strings.HasPrefix(lines[i], title) {
			i++
		}
		if i == len(lines) || lipgloss.Width(lines[i]) != 80 {
			t.Fatalf("the panels are stacked on the whole width, %q after line %d:\n%s", title, at, view)
		}
		at = i
	}
	if strings.Contains(view, "TITLE") || strings.Contains(view, "CPLX") {
		t.Errorf("the compact list has no column header:\n%s", view)
	}
	for _, row := range []string{"▸ 14 #55 implement artifacts: accept test_file", "15 #54 plan tui: delamain watch"} {
		if !has(view, row) {
			t.Errorf("the list shows RUN ISSUE STATE TITLE, want %q:\n%s", row, view)
		}
	}
	if !has(view, "[all] delm chec plan impl revi") {
		t.Errorf("the chips are cut to 4 letters:\n%s", view)
	}
	if !has(view, "following · G 1/1 · 100%") {
		t.Errorf("the logs footer is short:\n%s", view)
	}
	if !strings.HasPrefix(lines[len(lines)-2], "╰") || !strings.Contains(panelText(view, "logs · run 14"), "following · G") {
		t.Errorf("the logs take the rest of the height, down to the footer:\n%s", view)
	}

	m = send(t, m, keyPress("3"))
	if view := screen(m); !has(view, "all delm [chec] plan impl revi") {
		t.Errorf("3 picks checks:\n%s", view)
	}
}

// statusRun has every field of the status, with a title longer than any small layout.
func statusRun() RunView {
	v := active(14, "#55", "implement", "low", "artifacts: accept test_file and format_check in .delamain.yaml so that every gate reads them")
	v.Harness, v.Branch, v.Base = "claude", "fix/55-artifacts-accept-test-file-and-format-check", "main"
	v.Owner = Process{PID: 51230, Alive: true}
	v.PR = "https://github.com/paraizofelipe/delamain/pull/61"
	ended := watchNow.Add(-time.Hour + 131*time.Second)
	v.Agents = []AgentView{
		{Role: "planner", State: "done", StartedAt: watchNow.Add(-time.Hour), EndedAt: &ended, Tokens: 41_000, CostUSD: 0.38},
		{Role: "implementer", State: "running", StartedAt: watchNow.Add(-402 * time.Second), Tokens: 141_000, CostUSD: 1.56},
	}
	return v
}

func TestWatchShouldNotLoseStatusFieldsInSmallLayouts(t *testing.T) {
	run := statusRun()
	done := finished(12, "#49", "approved: merged", "logs: colors")
	src := &fakeWatchSource{
		active: []RunView{run},
		done:   []RunView{done},
		status: map[int64]RunStatus{14: {Run: run, Budget: BudgetView{Tokens: 182_000, CostUSD: 1.94}}, 12: {Run: done, Budget: BudgetView{Tokens: 9_000, CostUSD: 0.12}}},
	}
	m := resize(t, startWatch(t, src, 160), 80, 27)
	strip := panelText(screen(m), "status · enter details")
	if n := len(strings.Split(strip, "\n")); n < 4 {
		t.Errorf("the strip wraps its 3 lines instead of cutting them, %d lines:\n%s", n, strip)
	}
	for _, want := range []string{
		"run 14 #55 artifacts: accept test_file and format_check in .delamain.yaml so that every gate reads them",
		"implement · iter 1/3 · low · pid 51230 (alive)",
		"182K tokens · US$ 1.94 · implementer 6m42s · paraizofelipe/delamain/pull/61",
	} {
		if !strings.Contains(words(strip), want) {
			t.Errorf("the compact strip misses %q:\n%s", want, strip)
		}
	}
	if strings.Contains(strip, "…") {
		t.Errorf("the strip cuts nothing:\n%s", strip)
	}

	next, _ := m.Update(keyPress("j")) // the reload of its status has not ended
	if strip := words(panelText(screen(next), "status · enter details")); !strings.Contains(strip, "- tokens · US$ - · approved: merged") || strings.Contains(strip, "pull") {
		t.Errorf("an ended run without its status read shows - and its result:\n%s", strip)
	}
	if strip := words(panelText(screen(send(t, m, keyPress("j"))), "status · enter details")); !strings.Contains(strip, "9K tokens · US$ 0.12 · approved: merged") {
		t.Errorf("then its spend:\n%s", strip)
	}
}

// numberedWords is n words "w001 w002 …", 5 columns each.
func numberedWords(n int) string {
	ws := make([]string, n)
	for i := range ws {
		ws[i] = fmt.Sprintf("w%03d", i+1)
	}
	return strings.Join(ws, " ")
}

// wrapped is the text of the log row starting with prefix and of the rows after it that start
// with indent spaces then text: the continuations of a wrapped line, joined without spaces.
func wrapped(t *testing.T, rows []string, prefix string, indent int) (text string, n int) {
	t.Helper()
	for i, r := range rows {
		if !strings.HasPrefix(r, prefix) {
			continue
		}
		text, n = r[len(prefix):], 1
		for _, c := range rows[i+1:] {
			if !strings.HasPrefix(c, strings.Repeat(" ", indent)) || strings.HasPrefix(c, strings.Repeat(" ", indent+1)) {
				break
			}
			text, n = text+c[indent:], n+1
		}
		return strings.ReplaceAll(text, " ", ""), n
	}
	t.Fatalf("no log row starts with %q:\n%s", prefix, strings.Join(rows, "\n"))
	return "", 0
}

func TestWatchShouldWrapLogLinesInCompactAndMini(t *testing.T) {
	v := logRun(t, 14, "#55")
	long := numberedWords(40) // 200 columns
	writeLog(t, v, "implementer", "12:46:58 💬 "+long+"\n12:46:59 ✗ failed "+long+"\n")
	writeLog(t, v, "checks", "--- FAIL: TestGates "+long+"\n")
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 80, true)
	rows := panelRows(screen(m), "logs · run 14")

	text, n := wrapped(t, rows, "implementer │ 12:46:58 ", 23)
	if want := strings.ReplaceAll("💬 "+long, " ", ""); text != want || n < 4 {
		t.Errorf("the 200 columns line is whole on %d rows, its continuations under its text:\n%s", n, strings.Join(rows, "\n"))
	}
	text, n = wrapped(t, rows, "checks      │ ", 14)
	if want := strings.ReplaceAll("--- FAIL: TestGates "+long, " ", ""); text != want || n < 4 {
		t.Errorf("a line without a time wraps right after │, on %d rows:\n%s", n, strings.Join(rows, "\n"))
	}
	if strings.Contains(strings.Join(rows, "\n"), "…") {
		t.Errorf("no log line is cut:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[len(rows)-1], "3/3 · 100%") {
		t.Errorf("the footer counts lines, not rows: %q", rows[len(rows)-1])
	}
	// The ✗ line is red: so are its continuations, and the color stops before the border.
	for _, w := range []string{"w001", "w020", "w040"} {
		row := ""
		for _, l := range strings.Split(m.View(), "\n") {
			if strings.Contains(l, w) && strings.Contains(l, "\x1b[31m") {
				row = l
			}
		}
		at := strings.Index(row, w)
		if seqs := sgr.FindAllString(row[:max(at, 0)], -1); at < 0 || len(seqs) == 0 || !strings.Contains(seqs[len(seqs)-1], "31") {
			t.Errorf("%s of the ✗ line keeps its red:\n%q", w, row)
			continue
		}
		if !strings.Contains(row[at:strings.LastIndex(row, "│")], "\x1b[0m") {
			t.Errorf("the red of %s stops before the border:\n%q", w, row)
		}
	}
}

func TestWatchLogsShouldWrapOnlyTheLinesThatFitOnScreen(t *testing.T) {
	v := logRun(t, 14, "#55")
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 80, false)
	for r := range 4 { // 1000 lines of 4 KiB, 250 a reload
		var b strings.Builder
		for i := range 250 {
			fmt.Fprintf(&b, "12:00:00 %s END-%04d\n", strings.Repeat("word ", 800), r*250+i)
		}
		writeLog(t, v, "implementer", b.String())
		m = reload(t, m)
	}
	var view string
	if n := allocated(func() { view = m.View() }); n > mib {
		t.Fatalf("a frame of 1000 lines of 4 KiB allocated %d KiB: every line is wrapped", n>>10)
	}
	if !strings.Contains(stripANSI(view), "END-0999") || !strings.Contains(stripANSI(view), "1000/1000 · 100%") {
		t.Fatalf("the end of the last line is on screen:\n%s", stripANSI(view))
	}
	m = send(t, send(t, m, keyPress("tab")), keyPress("g"))
	if n := allocated(func() { view = m.View() }); n > mib {
		t.Fatalf("a paused frame allocated %d KiB: every line is wrapped", n>>10)
	}
	if !strings.Contains(stripANSI(view), "implementer │ 12:00:00 word word") || strings.Contains(stripANSI(view), "END-0000") {
		t.Fatalf("paused at the top, the first line starts the panel:\n%s", stripANSI(view))
	}
}

// nospace is s without spaces: a wrapped text, joined, equals it whatever the breaks.
func nospace(s string) string { return strings.Join(strings.Fields(s), "") }

func TestWatchShouldOpenTheFullStatusWithEnterAndCloseWithEsc(t *testing.T) {
	v := statusRun()
	v.RunDir = logRun(t, 14, "#55").RunDir
	writeLog(t, v, "implementer", "12:46:58 💬 adjusted the fixture\n")
	src := &fakeWatchSource{active: []RunView{v}, status: map[int64]RunStatus{14: {Run: v, Budget: BudgetView{Summary: "182K de 4M tokens · US$ 1.94"}}}}
	m := resize(t, startLogs(t, src, 160, false), 80, 27)
	logs := screen(m)
	if view := screen(send(t, m, keyPress("esc"))); view != logs {
		t.Fatalf("esc without the full status does nothing:\n%s", view)
	}

	m = send(t, m, keyPress("enter"))
	view := screen(m)
	if strings.Contains(view, "╭ logs") || strings.Contains(view, "╭ status · enter details") || !strings.Contains(view, "╭ runs") {
		t.Fatalf("enter puts the full status where the logs and the strip were, under the list:\n%s", view)
	}
	full := panelText(view, "status · run 14 ")
	for _, want := range []string{
		"RUN 14 · #55 artifacts: accept test_file and format_check in .delamain.yaml so that every gate reads them",
		"HARNESS claude", "STATE implement", "INTERACTIONS 1/3", "COMPLEXITY low",
		"BRANCH fix/55-artifacts-accept-test-file-and-format-check (base main)", "OWNER pid 51230 (alive)",
		"BUDGET 182K de 4M tokens · US$ 1.94", "PR/MR https://github.com/paraizofelipe/delamain/pull/61",
		"planner done 2m11s 41K · 0.38", "implementer running 6m42s 141K · 1.56",
	} {
		if !strings.Contains(nospace(full), nospace(want)) {
			t.Errorf("the full status misses %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "…") {
		t.Errorf("the fields of the full status wrap instead of being cut:\n%s", full)
	}

	for _, keys := range [][]string{{"esc"}, {"enter"}} {
		back := m
		for _, k := range keys {
			back = send(t, back, keyPress(k))
		}
		if view := screen(back); view != logs {
			t.Errorf("%s goes back to the logs:\n%s", keys, view)
		}
	}

	wide := startLogs(t, src, 160, false)
	if screen(send(t, wide, keyPress("enter"))) != screen(wide) {
		t.Errorf("the wide layout shows the full status already: enter does nothing")
	}
}

func TestWatchShouldSplitTheWideLayoutFortyPercentLeft(t *testing.T) {
	src := crowded()
	for width, left := range map[int]int{140: 56, 160: 64, 200: 72} {
		lines := strings.Split(screen(startWatch(t, src, width)), "\n")
		runs := lines[1]
		if !strings.HasPrefix(runs, "╭ runs ") || lipgloss.Width(runs[:strings.Index(runs, "╮")+len("╮")]) != left {
			t.Errorf("width %d: the runs panel is %d columns wide: %q", width, left, runs)
		}
		active := 0
		for _, l := range lines {
			if strings.Contains(l, " PR#") && strings.Contains(l, "deliver_draft") {
				active++
			}
		}
		if active != 8 || !strings.Contains(screen(startWatch(t, src, width)), "FINISHED · 22") {
			t.Errorf("width %d: every active run is listed, %d of 8:\n%s", width, active, strings.Join(lines, "\n"))
		}
	}
}

func TestWatchShouldStripControlSequencesFromRunText(t *testing.T) {
	evil := "evil\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mforged\u009b6n\x00\x7f\ttab"
	v := active(1, "#1\x1b[1m", "implement\x1b[5m", "low", evil)
	v.Branch, v.Base, v.PR, v.Harness, v.Kind = "fix/"+evil, "main"+evil, "https://x/"+evil, "omp"+evil, "issue"+evil
	v.Agents = []AgentView{{Role: "planner" + evil, State: "done" + evil, StartedAt: watchNow}}
	done := finished(2, "#2"+evil, "approved"+evil+": ok", evil)
	src := &fakeWatchSource{active: []RunView{v}, done: []RunView{done}, status: map[int64]RunStatus{1: {Run: v, Budget: BudgetView{Summary: evil}}}}
	m := startWatch(t, src, 160)
	for _, view := range []string{m.View(), send(t, m, keyPress("j")).View()} {
		if i := strings.IndexFunc(view, func(r rune) bool { return r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) }); i >= 0 {
			t.Fatalf("control character %q reached the terminal:\n%q", view[i], view)
		}
		if !strings.Contains(view, "evil]52;c;ZXZpbA==[2J[31mforged") {
			t.Fatalf("only the control characters go, the text stays:\n%s", view)
		}
		for i, l := range strings.Split(view, "\n") {
			if w := lipgloss.Width(l); w > 160 {
				t.Fatalf("line %d is %d wide", i, w)
			}
		}
	}
	src.err = errors.New("read " + evil)
	if view := reload(t, m).View(); strings.ContainsAny(view, "\x1b\x07") {
		t.Fatalf("an error message is text from outside too:\n%q", view)
	}
}

// logRun is an active run whose logs live in a temporary run dir.
func logRun(t *testing.T, id int64, issue string) RunView {
	t.Helper()
	v := active(id, issue, "implement", "low", "logs")
	v.RunDir = t.TempDir()
	return v
}

// writeLog appends text to the run's logs/<source>.log.
func writeLog(t *testing.T, v RunView, source, text string) {
	t.Helper()
	dir := filepath.Join(v.RunDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, source+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // test dir
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// startLogs is startWatch, with colors forced on when colored, after the first tick: the logs of
// the run selected by the first reload are read from then on.
func startLogs(t *testing.T, src *fakeWatchSource, width int, colored bool) tea.Model {
	t.Helper()
	st := noColors()
	if colored {
		st = lineStyles(termenv.ANSI)
	}
	m := tea.Model(newWatchModel(src, st, watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: 40})
	return reload(t, settle(t, m, m.Init()))
}

func TestWatchLogsShouldShowTheSelectedRunLogsColoredLikeDelamainLogs(t *testing.T) {
	v := logRun(t, 14, "#55")
	long := "12:46:58 💬 " + strings.Repeat("the test of gates used test_file with the old key ", 4)
	writeLog(t, v, "delamain", "12:44:19 gate run=14 name=test exit=1\n12:44:19 WARN verify failed run=14 gate=test iteration=1\n")
	writeLog(t, v, "implementer", "12:44:20 ▶ prompt (1380 caracteres)\n"+long+"\n")
	writeLog(t, v, "checks", "12:44:19 ■ test: código 1\n--- FAIL: TestGates (0.00s)\n")
	st := lineStyles(termenv.ANSI)
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 160, true)

	want := []logLine{
		{"12:44:19", "checks", "12:44:19 ■ test: código 1"},
		{"12:44:19", "checks", "--- FAIL: TestGates (0.00s)"},
		{"12:44:19", "delamain", "12:44:19 gate run=14 name=test exit=1"},
		{"12:44:19", "delamain", "12:44:19 WARN verify failed run=14 gate=test iteration=1"},
		{"12:44:20", "implementer", "12:44:20 ▶ prompt (1380 caracteres)"},
		{"12:46:58", "implementer", long},
	}
	view := m.View()
	inner := 160 - 64 - 4 // the logs panel takes what the 64 columns of the left one leave
	at := -1
	for _, l := range want {
		var out bytes.Buffer
		if err := emit(&out, st, []logLine{l}); err != nil {
			t.Fatal(err)
		}
		row := ansi.Truncate(strings.TrimSuffix(out.String(), "\n"), inner, "…")
		i := strings.Index(view, row)
		if i <= at {
			t.Fatalf("want the line of `delamain logs`, in time order, cut at %d columns:\n%q\nin\n%s", inner, row, stripANSI(view))
		}
		at = i
	}
	if !strings.Contains(stripANSI(view), "…") {
		t.Errorf("a line wider than the panel ends in …:\n%s", stripANSI(view))
	}
}

func TestWatchLogsShouldAppendLinesWrittenAfterTheStart(t *testing.T) {
	v := logRun(t, 14, "#55")
	writeLog(t, v, "delamain", "12:44:19 state run=14 state=plan\n")
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 160, false)
	if view := screen(m); !strings.Contains(view, "12:44:19 state run=14 state=plan") {
		t.Fatalf("the logs written before the start are shown:\n%s", view)
	}

	writeLog(t, v, "implementer", "12:44:20 ▶ prompt (900 caracteres)\n")
	if strings.Contains(screen(m), "▶ prompt") {
		t.Fatal("nothing is read outside a tick")
	}
	view := screen(reload(t, m))
	if !strings.Contains(view, "implementer │ 12:44:20 ▶ prompt (900 caracteres)") {
		t.Fatalf("a line written after the start shows up on the next tick:\n%s", view)
	}
	if strings.Count(view, "12:44:19 state run=14 state=plan") != 1 {
		t.Fatalf("lines already shown are not read again:\n%s", view)
	}
}

func TestWatchLogsShouldSayThereAreNoLogsYetWhenTheRunHasNone(t *testing.T) {
	v := logRun(t, 14, "#55")
	view := screen(startLogs(t, &fakeWatchSource{active: []RunView{v}}, 200, false))
	if want := "no logs yet in " + filepath.Join(v.RunDir, "logs"); !strings.Contains(view, want) {
		t.Fatalf("a run without logs says where they will be: %q\n%s", want, view)
	}
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 200, false)
	for height := 6; height <= 9; height++ {
		if lines := strings.Split(send(t, m, tea.WindowSizeMsg{Width: 200, Height: height}).View(), "\n"); len(lines) > height {
			t.Fatalf("%d lines high: %d lines", height, len(lines))
		}
	}
}

func TestWatchLogsShouldStripControlSequencesFromLogText(t *testing.T) {
	evil := "evil\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mforged\u009b6n\x00\x7f\ttab"
	v := logRun(t, 14, "#55")
	writeLog(t, v, "delamain", "12:44:19 state run=14 "+evil+"\n")
	empty := active(15, "#56", "plan", "", "x")
	empty.RunDir = filepath.Join(t.TempDir(), "run"+evil)
	m := startLogs(t, &fakeWatchSource{active: []RunView{v, empty}}, 200, true)
	for _, view := range []string{m.View(), send(t, m, keyPress("j")).View()} {
		if i := strings.IndexFunc(stripANSI(view), func(r rune) bool { return r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) }); i >= 0 {
			t.Fatalf("control character %q reached the terminal:\n%q", stripANSI(view)[i], view)
		}
		if strings.Contains(view, "\x1b]") || strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x07") {
			t.Fatalf("an escape sequence of the log reached the terminal:\n%q", view)
		}
	}
	if view := screen(m); !strings.Contains(view, "evil]52;c;ZXZpbA==[2J[31mforged") {
		t.Fatalf("only the control characters go, the text stays:\n%s", view)
	}
	if view := screen(send(t, m, keyPress("j"))); !strings.Contains(view, "no logs yet in "+filepath.Join(filepath.Dir(empty.RunDir), "run")+"evil]52;c;") {
		t.Fatalf("the dir of an empty run is cleaned too:\n%s", view)
	}
}

func TestWatchLogsShouldResetWhenTheSelectionChanges(t *testing.T) {
	a, b := logRun(t, 1, "#11"), logRun(t, 2, "#12")
	writeLog(t, a, "delamain", "12:00:01 line of run one\n")
	writeLog(t, b, "delamain", "12:00:02 line of run two\n")
	src := &fakeWatchSource{active: []RunView{a, b}}
	m := startLogs(t, src, 160, false)
	if view := screen(m); !strings.Contains(view, "line of run one") || strings.Contains(view, "line of run two") {
		t.Fatalf("the first run's logs are shown:\n%s", view)
	}

	view := screen(send(t, m, keyPress("j")))
	if !strings.Contains(view, "line of run two") || strings.Contains(view, "line of run one") {
		t.Fatalf("another run clears the panel and shows its own logs:\n%s", view)
	}

	// A reload that read run one's new lines arrives after the selection moved to run two.
	writeLog(t, a, "delamain", "12:00:03 late line of run one\n")
	m, cmd := m.Update(watchTickMsg{})
	_, inFlight := split(t, messages(cmd))
	m = send(t, m, keyPress("j"))
	for _, msg := range inFlight {
		m = send(t, m, msg)
	}
	view = screen(m)
	if strings.Contains(view, "run one") || !strings.Contains(view, "line of run two") {
		t.Fatalf("lines read for a run no longer selected are dropped:\n%s", view)
	}
	if view := screen(send(t, m, keyPress("k"))); strings.Contains(view, "line of run two") || strings.Count(view, "run one") != 2 {
		t.Fatalf("back on run one, its logs are read again from the start:\n%s", view)
	}
}

func TestWatchLogsShouldFlushThePartialLineWhenTheRunEnds(t *testing.T) {
	v := logRun(t, 14, "#55")
	writeLog(t, v, "delamain", "12:00:01 state run=14 state=deliver\n12:00:02 delivered run=14 url=https://x/pull/1")
	src := &fakeWatchSource{active: []RunView{v}}
	m := reload(t, startLogs(t, src, 160, true))
	if view := screen(m); !strings.Contains(view, "state=deliver") || strings.Contains(view, "delivered") {
		t.Fatalf("an active run's unterminated line waits for its end:\n%s", view)
	}
	if strings.Contains(screen(m), "ended") {
		t.Fatalf("an active run has not ended:\n%s", screen(m))
	}

	done := finished(14, "#55", "approved: merged", "logs")
	done.RunDir = v.RunDir
	src.active, src.done = nil, []RunView{done}
	m = reload(t, reload(t, m))
	view := screen(m)
	if !strings.Contains(view, "delamain    │ 12:00:02 delivered run=14 url=https://x/pull/1") {
		t.Fatalf("once the run ended its last line is shown:\n%s", view)
	}
	if title := line(m.View(), "logs · run 14"); !strings.Contains(title, "logs · run 14 · \x1b[2mended\x1b[0m") {
		t.Fatalf("the title says the run ended, in muted: %q", title)
	}
	if row := line(m.View(), "ended · "); !strings.Contains(row, "\x1b[2mended · \x1b[0m\x1b[32mapproved\x1b[0m: merged") {
		t.Fatalf("the panel footer shows the result in its color: %q\n%s", row, view)
	}
	if view := screen(reload(t, m)); strings.Count(view, "delivered run=14") != 1 {
		t.Fatalf("the flushed line is not read again:\n%s", view)
	}
}

// sgrBefore is the parameters of the last SGR sequence before text in s, "" when text is not
// preceded by one.
func sgrBefore(s, text string) string {
	i := strings.Index(s, text)
	if i < 0 || !strings.HasSuffix(s[:i], "m") {
		return ""
	}
	start := strings.LastIndex(s[:i], "\x1b[")
	return s[start+2 : i-1]
}

func TestWatchLogsShouldFilterBySourceWithoutDroppingLines(t *testing.T) {
	a, b := logRun(t, 1, "#11"), logRun(t, 2, "#12")
	writeLog(t, a, "delamain", "12:00:01 state run=1 state=verify\n12:00:03 gate run=1 name=test exit=1\n")
	writeLog(t, a, "checks", "12:00:02 ▶ test: go test ./...\n")
	writeLog(t, a, "implementer", "12:00:04 ▶ prompt (900 caracteres)\n")
	writeLog(t, b, "checks", "12:00:05 ▶ lint: make lint\n")
	writeLog(t, b, "planner", "12:00:06 ▶ prompt (100 caracteres)\n")
	m := startLogs(t, &fakeWatchSource{active: []RunView{a, b}}, 160, true)

	view := screen(m)
	if !has(view, "source [all] delamain checks planner implementer reviewer") {
		t.Fatalf("the chips of every source are above the logs, all active:\n%s", view)
	}
	if !strings.Contains(view, "4 of 4 lines") {
		t.Fatalf("the footer counts the lines shown of the lines read:\n%s", view)
	}

	m = send(t, m, keyPress("3"))
	view = screen(m)
	if !has(view, "source all delamain [checks] planner implementer reviewer") {
		t.Fatalf("3 picks checks:\n%s", view)
	}
	if sgr := sgrBefore(m.View(), "[checks]"); !strings.Contains(sgr, "7") || !strings.Contains(sgr, "33") {
		t.Fatalf("the active chip is in reverse video in the source color, got SGR %q", sgr)
	}
	if !strings.Contains(view, "checks      │ 12:00:02 ▶ test: go test ./...") || strings.Contains(view, "state=verify") || strings.Contains(view, "▶ prompt") {
		t.Fatalf("only the checks lines are shown:\n%s", view)
	}
	if !strings.Contains(view, "1 of 4 lines") {
		t.Fatalf("the filter only hides lines:\n%s", view)
	}

	view = screen(send(t, m, keyPress("j")))
	if !has(view, "source all delamain [checks] planner implementer reviewer") || !strings.Contains(view, "▶ lint: make lint") || strings.Contains(view, "▶ prompt") {
		t.Fatalf("the filter stays on for the next run:\n%s", view)
	}

	m = send(t, m, keyPress("1"))
	view = screen(m)
	for _, want := range []string{"state=verify", "▶ test: go test", "name=test exit=1", "▶ prompt (900 caracteres)", "4 of 4 lines"} {
		if !strings.Contains(view, want) {
			t.Fatalf("1 shows every line again, %q missing:\n%s", want, view)
		}
	}
	if sgr := sgrBefore(m.View(), "[all]"); sgr != "7" {
		t.Fatalf("the all chip is in reverse video with no color, got SGR %q", sgr)
	}
	for k, chip := range map[string]string{"2": "[delamain]", "4": "[planner]", "5": "[implementer]", "6": "[reviewer]"} {
		if view := screen(send(t, m, keyPress(k))); !strings.Contains(view, chip) {
			t.Fatalf("%s picks %s:\n%s", k, chip, view)
		}
	}
}

// numbered writes the lines "line <from>" to "line <to>" to the run's delamain.log.
func numbered(t *testing.T, v RunView, from, to int) {
	t.Helper()
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "12:%02d:%02d line %03d\n", i/60, i%60, i)
	}
	writeLog(t, v, "delamain", b.String())
}

// shows reports whether the view has the lines "line <from>" to "line <to>" and not the ones
// around them.
func shows(view string, from, to int) bool {
	for i := from - 1; i <= to+1; i++ {
		if strings.Contains(view, fmt.Sprintf("line %03d", i)) != (i >= from && i <= to) {
			return false
		}
	}
	return true
}

func TestWatchLogsShouldPauseWhenScrollingUpAndResumeWithG(t *testing.T) {
	v := logRun(t, 14, "#55")
	numbered(t, v, 1, 100)
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 160, true)
	// 40 lines: header, footer, borders, chips, two separators and the panel footer leave 32.
	step := func(name string, from, to int, state, footer string) {
		t.Helper()
		view := screen(m)
		if !shows(view, from, to) {
			t.Fatalf("%s: want lines %d to %d:\n%s", name, from, to, view)
		}
		if !strings.Contains(line(view, "logs · run 14"), "logs · run 14 · "+state+" ") {
			t.Fatalf("%s: title %q, want %q", name, line(view, "logs · run 14"), state)
		}
		if !has(view, footer) {
			t.Fatalf("%s: panel footer %q, want %q", name, line(view, " lines · "), footer)
		}
	}
	step("start", 69, 100, "● following", "following the end · G 100 of 100 lines · 100%")
	if sgr := sgrBefore(m.View(), "● following"); sgr != "32" {
		t.Fatalf("following is in ok, got SGR %q", sgr)
	}

	m = send(t, m, keyPress("pgup"))
	step("pgup goes up half a page", 53, 84, "paused", "paused · G back to the end 100 of 100 lines · 76%")
	if sgr := sgrBefore(m.View(), "paused"); sgr != "33" {
		t.Fatalf("paused is in warn, got SGR %q", sgr)
	}
	numbered(t, v, 101, 101)
	m = reload(t, m)
	step("new lines keep a paused view", 53, 84, "paused", "paused · G back to the end 101 of 101 lines · 75%")
	m = send(t, m, keyPress("ctrl+u"))
	step("ctrl+u goes up half a page", 37, 68, "paused", "paused · G back to the end 101 of 101 lines · 52%")
	m = send(t, m, keyPress("pgdown"))
	step("pgdown goes down half a page", 53, 84, "paused", "paused · G back to the end 101 of 101 lines · 75%")
	for range 3 {
		m = send(t, m, keyPress("ctrl+d"))
	}
	step("the end reached by scrolling stays paused", 70, 101, "paused", "paused · G back to the end 101 of 101 lines · 100%")

	m = send(t, m, keyPress("f"))
	numbered(t, v, 102, 102)
	m = reload(t, m)
	step("f follows again", 71, 102, "● following", "following the end · G 102 of 102 lines · 100%")
	m = send(t, m, keyPress("f"))
	numbered(t, v, 103, 103)
	m = reload(t, m)
	step("f pauses where the view is", 71, 102, "paused", "paused · G back to the end 103 of 103 lines · 99%")
	m = send(t, m, keyPress("f"))
	step("f goes back to the end", 72, 103, "● following", "following the end · G 103 of 103 lines · 100%")

	m = send(t, send(t, m, keyPress("tab")), keyPress("pgup"))
	step("pgup with the logs focused", 56, 87, "paused", "paused · G back to the end 103 of 103 lines · 77%")
	m = send(t, m, keyPress("G"))
	step("G goes back to the end", 72, 103, "● following", "following the end · G 103 of 103 lines · 100%")
	m = send(t, send(t, m, keyPress("ctrl+u")), keyPress("end"))
	step("end goes back to the end", 72, 103, "● following", "following the end · G 103 of 103 lines · 100%")
}

// border is the SGR parameters of the top left corner of the panel titled title.
func border(view, title string) string {
	return sgrBefore(view, "╭\x1b[0m "+title)
}

func TestWatchShouldToggleFocusWithTabAndZoomWithZ(t *testing.T) {
	v := logRun(t, 14, "#55")
	writeLog(t, v, "delamain", "12:00:01 state run=14 state=plan\n")
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 160, true)
	look := func(name, runs, logs string) {
		t.Helper()
		view := m.View()
		if got := border(view, "runs"); got != runs {
			t.Fatalf("%s: runs border SGR %q, want %q", name, got, runs)
		}
		if got := border(view, "logs · run 14"); got != logs {
			t.Fatalf("%s: logs border SGR %q, want %q", name, got, logs)
		}
		if got := border(view, "status · run 14"); got != "2" {
			t.Fatalf("%s: the status panel never has the focus, SGR %q", name, got)
		}
	}
	look("start", "36", "2")
	m = send(t, m, keyPress("tab"))
	look("tab", "2", "36")
	m = send(t, m, keyPress("tab"))
	look("tab again", "36", "2")

	m = send(t, m, keyPress("z"))
	view := screen(m)
	if strings.Contains(view, "╭ runs") || strings.Contains(view, "╭ status") || strings.Contains(view, "#55") {
		t.Fatalf("z hides the list and the status:\n%s", view)
	}
	if top := strings.Split(view, "\n")[1]; !strings.HasPrefix(top, "╭ logs · run 14") || lipgloss.Width(top) != 160 {
		t.Fatalf("z gives the logs the whole width: %q", top)
	}
	if border(m.View(), "logs · run 14") != "36" {
		t.Fatalf("z moves the focus to the logs:\n%q", m.View())
	}
	if got := strings.TrimSpace(footer(m)); got != "↑↓ navigate  tab focus  1-6 source  f follow  z exit full screen  q quit" {
		t.Fatalf("full screen footer = %q", got)
	}
	m = send(t, m, keyPress("z"))
	if view := screen(m); !strings.Contains(view, "╭ runs") || !strings.Contains(view, "╭ status · run 14") {
		t.Fatalf("another z brings the list back:\n%s", view)
	}
	look("z twice", "36", "2")

	m = send(t, send(t, send(t, m, keyPress("tab")), keyPress("z")), keyPress("z"))
	look("z twice from the logs", "2", "36")
}

func TestWatchLogsShouldScrollByLineOnlyWhenTheLogsHaveFocus(t *testing.T) {
	a := logRun(t, 1, "#11")
	numbered(t, a, 1, 100)
	src := &fakeWatchSource{active: []RunView{a, logRun(t, 2, "#12")}}
	m := startLogs(t, src, 160, false)
	selected := func(name, issue string) {
		t.Helper()
		if row := line(screen(m), "▸"); !strings.Contains(row, " "+issue+" ") {
			t.Fatalf("%s: the selection is %s, got %q", name, issue, row)
		}
	}
	m = send(t, m, keyPress("j"))
	selected("j on the list", "#12")
	m = send(t, m, keyPress("k"))
	selected("k on the list", "#11")
	if view := screen(m); !shows(view, 69, 100) {
		t.Fatalf("the list keys move the selection, the logs follow the end:\n%s", view)
	}

	m = send(t, m, keyPress("tab"))
	for _, s := range []struct {
		key      string
		from, to int
		footer   string
	}{
		{"k", 68, 99, "paused · G back to the end 100 of 100 lines · 99%"},
		{"up", 67, 98, "paused · G back to the end 100 of 100 lines · 97%"},
		{"j", 68, 99, "paused · G back to the end 100 of 100 lines · 99%"},
		{"down", 69, 100, "paused · G back to the end 100 of 100 lines · 100%"},
		{"down", 69, 100, "paused · G back to the end 100 of 100 lines · 100%"},
		{"g", 1, 32, "paused · G back to the end 100 of 100 lines · 0%"},
		{"G", 69, 100, "following the end · G 100 of 100 lines · 100%"},
		{"home", 1, 32, "paused · G back to the end 100 of 100 lines · 0%"},
		{"end", 69, 100, "following the end · G 100 of 100 lines · 100%"},
	} {
		m = send(t, m, keyPress(s.key))
		view := screen(m)
		if !shows(view, s.from, s.to) || !has(view, s.footer) {
			t.Fatalf("%s with the logs focused: want lines %d to %d and %q:\n%s", s.key, s.from, s.to, s.footer, view)
		}
		selected(s.key+" with the logs focused", "#11")
	}
}

func TestWatchLogsShouldKeepOnlyTheLast5000Lines(t *testing.T) {
	v := logRun(t, 14, "#55")
	numbered(t, v, 1, 5003)
	m := startLogs(t, &fakeWatchSource{active: []RunView{v}}, 160, false)
	if view := screen(m); !has(view, "following the end · G 5000 of 5000 lines · 100%") || !strings.Contains(view, " line 5003 ") {
		t.Fatalf("the last 5000 lines are kept:\n%s", view)
	}
	m = send(t, send(t, m, keyPress("tab")), keyPress("g"))
	view := screen(m)
	if !has(view, "… 3 earlier lines omitted") || !strings.Contains(view, " line 004 ") || strings.Contains(view, " line 003 ") {
		t.Fatalf("the first lines are dropped and counted at the top:\n%s", view)
	}

	numbered(t, v, 5004, 5005)
	view = screen(reload(t, m))
	if !has(view, "… 5 earlier lines omitted") || !strings.Contains(view, " line 006 ") || strings.Contains(view, " line 005 ") || !strings.Contains(view, "5000 of 5000 lines") {
		t.Fatalf("new lines push the oldest out:\n%s", view)
	}
}

// allocated is the heap bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// held is the heap bytes still in use once m is built by f, after a collection.
func held(f func() tea.Model) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	m := f()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(m)
	return max(after.HeapAlloc, before.HeapAlloc) - before.HeapAlloc
}

const mib = 1 << 20

func TestWatchLogsShouldBoundTheMemoryOfAGiantLine(t *testing.T) {
	v := logRun(t, 14, "#55")
	writeLog(t, v, "implementer", "12:00:00 💬 "+strings.Repeat("x", 32*mib)+"\n12:00:01 after the giant line\n")
	m := startWatch(t, &fakeWatchSource{active: []RunView{v}}, 160)
	if n := allocated(func() { m = reload(t, m) }); n > 8*mib {
		t.Fatalf("reading a 32 MiB line allocated %d MiB", n/mib)
	}
	if view := screen(m); !strings.Contains(view, "12:00:01 after the giant line") {
		t.Fatalf("the lines after a giant one are shown:\n%s", view)
	}

	// A line that never ends grows a bit on every tick.
	for range 24 {
		writeLog(t, v, "implementer", strings.Repeat("y", 512<<10))
		if n := allocated(func() { m = reload(t, m) }); n > 4*mib {
			t.Fatalf("an unterminated line is held whole: a reload allocated %d MiB", n/mib)
		}
	}
	writeLog(t, v, "implementer", "\n12:00:02 after the endless line\n")
	if view := screen(reload(t, m)); !strings.Contains(view, "12:00:02 after the endless line") || !strings.Contains(view, "implementer │ yyy") {
		t.Fatalf("the endless line is shown cut, and the next ones after it:\n%s", view)
	}
}

func TestWatchLogsShouldHoldOnlyTheStartOfEachLongLine(t *testing.T) {
	v := logRun(t, 14, "#55")
	long := strings.Repeat("z", 64<<10)
	n := held(func() tea.Model {
		m := startWatch(t, &fakeWatchSource{active: []RunView{v}}, 160)
		for i := range 40 {
			var b strings.Builder
			for j := range 15 {
				fmt.Fprintf(&b, "12:00:%02d %03d-%02d %s\n", i%60, i, j, long)
			}
			writeLog(t, v, "delamain", b.String())
			m = reload(t, m)
		}
		if view := screen(m); !strings.Contains(view, "039-14 zzz") || !has(view, "following the end · G 600 of 600 lines · 100%") {
			t.Fatalf("every long line is kept, cut:\n%s", view)
		}
		return m
	})
	if n > 12*mib {
		t.Fatalf("600 lines of 64 KiB hold %d MiB: each line is held whole", n/mib)
	}
}

func TestWatchLogsShouldReadALargeBacklogInBoundedSteps(t *testing.T) {
	v := logRun(t, 14, "#55")
	var b strings.Builder
	for i := 1; i <= 200_000; i++ {
		fmt.Fprintf(&b, "12:00:00 backlog line %06d %s\n", i, strings.Repeat("b", 70))
	}
	writeLog(t, v, "delamain", b.String())
	m := startWatch(t, &fakeWatchSource{active: []RunView{v}}, 160)
	if n := allocated(func() { m = reload(t, m) }); n > 16*mib {
		t.Fatalf("a %d MiB backlog was read at once: %d MiB allocated", b.Len()/mib, n/mib)
	}
	view := screen(m)
	if !has(view, "following the end · G 5000 of 5000 lines · 100%") || !strings.Contains(view, "backlog line 200000 ") {
		t.Fatalf("the end of the backlog is shown:\n%s", view)
	}
	view = screen(send(t, send(t, m, keyPress("tab")), keyPress("g")))
	if !has(view, "… 195000 earlier lines omitted") || !strings.Contains(view, "backlog line 195001 ") || strings.Contains(view, "backlog line 195000 ") {
		t.Fatalf("the lines skipped are counted:\n%s", view)
	}
}
