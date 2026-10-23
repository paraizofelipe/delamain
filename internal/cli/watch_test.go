package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if got := strings.TrimSpace(footer(m)); got != "↑↓ navigate  q quit" {
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
	if got := strings.TrimSpace(footer(m)); got != "↑↓ navigate  q quit" || !strings.Contains(screen(m), "#12") {
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
	for _, width := range []int{80, 81, 99, 120, 137, 160, 179, 200} {
		for _, height := range []int{6, 15, 24, 40, 60} {
			src := crowded()
			for _, vs := range [][]RunView{src.active, src.done} {
				for i := range vs {
					vs[i].RunDir = logs.RunDir
				}
			}
			m := tea.Model(newWatchModel(src, lineStyles(termenv.ANSI), watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return watchNow }}))
			m = reload(t, settle(t, send(t, m, tea.WindowSizeMsg{Width: width, Height: height}), m.Init()))
			if height >= 15 && !strings.Contains(stripANSI(m.View()), "implementer │") {
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
				if height >= 15 && !strings.Contains(m.View(), "▸") {
					t.Fatalf("%dx%d after %q: the selected run stays visible:\n%s", width, height, k, stripANSI(m.View()))
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

func TestWatchShouldSplitTheWideLayoutFortyPercentLeft(t *testing.T) {
	src := crowded()
	for width, left := range map[int]int{80: 56, 160: 64, 200: 72} {
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
