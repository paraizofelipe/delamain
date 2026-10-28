package cli

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/paraizo/delamain/internal/store"
)

func queueOf(issues ...int) []store.QueueItem {
	out := make([]store.QueueItem, 0, len(issues))
	for i, n := range issues {
		out = append(out, store.QueueItem{ID: int64(i + 1), Position: i + 1, Repo: "/app", Issue: n})
	}
	return out
}

func TestQueueLinesShouldLayTheItemsOutAsAGridInOrder(t *testing.T) {
	m := watchModel{st: noColors(), data: watchData{queue: queueOf(12, 21, 30)}}
	got := m.queueLines(40)
	if want := " P:1 #12   P:2 #21   P:3 #30 "; len(got) != 1 || got[0] != want {
		t.Fatalf("one row of cells in execution order: %q, want %q", got, want)
	}
	if got := m.queueLines(20); len(got) != 2 || got[0] != " P:1 #12   P:2 #21 " || got[1] != " P:3 #30 " {
		t.Fatalf("the cells wrap at the panel's width: %q", got)
	}
}

func TestQueueLinesShouldKeepEveryRowForTheScroll(t *testing.T) {
	var issues []int
	for n := 1; n <= 30; n++ {
		issues = append(issues, 100+n)
	}
	m := watchModel{st: noColors(), data: watchData{queue: queueOf(issues...)}}
	got := m.queueLines(24) // two cells a row
	if len(got) != 15 || !strings.Contains(got[14], "P:30 #130") {
		t.Fatalf("no item is left out of the grid: %d rows, last %q", len(got), got[len(got)-1])
	}
	title, shown := m.queuePanel(got)
	if title != "queue · 30 · rows 1-3 of 15" || len(shown) != queueRows || !strings.Contains(shown[0], "P:1 #101") {
		t.Fatalf("the panel shows the first %d rows: %q %q", queueRows, title, shown)
	}
	m.queueScroll = 99
	if title, shown := m.queuePanel(got); title != "queue · 30 · rows 13-15 of 15" || !strings.Contains(shown[2], "P:30 #130") {
		t.Fatalf("the scroll stops at the last rows: %q %q", title, shown)
	}
}

func TestQueueLinesShouldSayWhenTheQueueIsEmpty(t *testing.T) {
	m := watchModel{st: noColors()}
	if got := m.queueLines(40); len(got) != 1 || got[0] != "empty" {
		t.Fatalf("empty queue: %q", got)
	}
}

func TestQueueCellsShouldHaveTheSlotBackground(t *testing.T) {
	m := watchModel{st: lineStyles(termenv.ANSI256), data: watchData{queue: queueOf(12)}}
	if got := m.queueLines(40)[0]; !strings.Contains(got, "48;5;62") {
		t.Fatalf("a taken position is painted on the slot background: %q", got)
	}
}

func TestQueueLinesShouldUseTheWaitingSlotStyleForWaitingItems(t *testing.T) {
	st := lineStyles(termenv.ANSI)
	items := queueOf(12, 21)
	items[0].Waiting = "#7 (link)"
	got := watchModel{st: st, data: watchData{queue: items}}.queueLines(40)
	if len(got) != 1 {
		t.Fatalf("both cells fit in a row: %q", got)
	}
	wantCodes(t, got[0], " P:1", " P:1 #12 ⏳ ", "43", "30", "1")
	ready := watchModel{st: st, data: watchData{queue: queueOf(12, 21)}}.queueLines(40)[0]
	want, _ := codesOf(ready, " P:1", " P:2 #21 ")
	if codes, _ := codesOf(got[0], " P:1", " P:2 #21 "); !maps.Equal(codes, want) || codes["43"] {
		t.Fatalf("a ready cell keeps the slot look %v, got %v:\n%q", want, codes, got[0])
	}
	if plain := stripANSI(got[0]); strings.Count(plain, "⏳") != 1 || !strings.HasSuffix(plain, " P:2 #21 ") {
		t.Fatalf("only the waiting cell is marked: %q", plain)
	}
}

func TestQueueLinesShouldWrapCorrectlyWithTheWaitingMarker(t *testing.T) {
	items := queueOf(12, 21)
	items[0].Waiting, items[1].Waiting = "#7 (link)", "#8 (corpo)"
	m := watchModel{st: noColors(), data: watchData{queue: items}}
	if got := m.queueLines(25); len(got) != 1 || got[0] != " P:1 #12 ⏳   P:2 #21 ⏳ " {
		t.Fatalf("two waiting cells, 12 columns each, fit in 25: %q", got)
	}
	if got := m.queueLines(24); len(got) != 2 || got[0] != " P:1 #12 ⏳ " || got[1] != " P:2 #21 ⏳ " {
		t.Fatalf("the ⏳ takes two columns, so 24 wraps them: %q", got)
	}
}

func TestWatchShouldShowTheQueueAboveTheStatus(t *testing.T) {
	for _, width := range []int{160, 120, 80} { // wide, medium, compact
		src := &fakeWatchSource{
			active: []RunView{active(1, "#1", "implement", "medium", "fix login")},
			status: map[int64]RunStatus{}, queue: queueOf(12, 21),
		}
		src.status[1] = RunStatus{Run: src.active[0]}
		view := stripANSI(startWatch(t, src, width).View())
		if got := panelText(view, "queue · 2"); !strings.Contains(got, "P:1 #12") || !strings.Contains(got, "P:2 #21") {
			t.Fatalf("%d columns: the queue panel shows its cells:\n%s", width, view)
		}
		lines := strings.Split(view, "\n")
		status, queue := -1, -1
		for i, l := range lines {
			if status < 0 && strings.Contains(l, "╭ status") {
				status = i
			}
			if strings.Contains(l, "╭ queue · 2") {
				queue = i
			}
		}
		if queue < 0 || status <= queue {
			t.Fatalf("%d columns: the queue is above the status:\n%s", width, view)
		}
	}
}

// queueWatch is the TUI with a queue of n items and three active runs.
func queueWatch(t *testing.T, width, height, n int) tea.Model {
	t.Helper()
	var issues []int
	for i := 1; i <= n; i++ {
		issues = append(issues, 100+i)
	}
	src := &fakeWatchSource{status: map[int64]RunStatus{}, queue: queueOf(issues...)}
	for id := int64(1); id <= 3; id++ {
		v := active(id, fmt.Sprintf("#%d", id), "implement", "medium", "fix login")
		src.active = append(src.active, v)
		src.status[id] = RunStatus{Run: v}
	}
	return send(t, startWatch(t, src, width), tea.WindowSizeMsg{Width: width, Height: height})
}

func TestWatchQueueShouldBeThreeRowsHighWhateverItHolds(t *testing.T) {
	for _, c := range []struct{ items, rows int }{{0, queueRows}, {2, queueRows}, {40, queueRows}} {
		for _, width := range []int{160, 120, 80} {
			view := stripANSI(queueWatch(t, width, 40, c.items).View())
			if rows := panelRows(view, "queue · "); len(rows) != c.rows {
				t.Fatalf("%d items, %d columns: the queue has %d rows, want %d:\n%s", c.items, width, len(rows), c.rows, view)
			}
		}
	}
}

func TestWatchQueueShouldScrollWithTheFocus(t *testing.T) {
	m := queueWatch(t, 80, 40, 40)
	rows := len(m.(watchModel).frame().queue)
	if rows <= queueRows {
		t.Fatalf("the grid must be taller than the panel: %d rows", rows)
	}
	m = send(t, m, keyPress("tab"))
	if !m.(watchModel).focusQueue {
		t.Fatal("tab moves the focus from the runs to the queue")
	}
	m = send(t, m, keyPress("j"))
	view := stripANSI(m.View())
	if !strings.Contains(view, fmt.Sprintf("queue · 40 · rows 2-4 of %d", rows)) || strings.Contains(panelText(view, "queue · "), "P:1 #101") {
		t.Fatalf("↓ scrolls the queue a row:\n%s", view)
	}
	if m.(watchModel).selectedID != 1 {
		t.Fatal("scrolling the queue leaves the selected run alone")
	}
	m = send(t, m, keyPress("G"))
	if view := stripANSI(m.View()); !strings.Contains(view, fmt.Sprintf("rows %d-%d of %d", rows-2, rows, rows)) || !strings.Contains(panelText(view, "queue · "), "P:40 #140") {
		t.Fatalf("G shows the last rows:\n%s", view)
	}
	m = send(t, m, keyPress("tab"))
	if wm := m.(watchModel); wm.focusQueue || !wm.focusLogs {
		t.Fatal("tab moves the focus from the queue to the logs")
	}
	m = send(t, m, keyPress("tab"))
	if wm := m.(watchModel); wm.focusQueue || wm.focusLogs {
		t.Fatal("tab moves the focus from the logs back to the runs")
	}
}

func TestWatchCompactShouldKeepTheRunsAheadOfTheQueue(t *testing.T) {
	view := stripANSI(queueWatch(t, 80, 18, 3).View())
	runs := panelText(view, "runs")
	for _, want := range []string{"#1", "#2", "#3"} {
		if !strings.Contains(runs, want) {
			t.Fatalf("a short terminal keeps every active run before the queue:\n%s", view)
		}
	}
	if strings.Contains(view, "queue · ") {
		t.Fatalf("with no room left for its three rows the queue is left out:\n%s", view)
	}
	if m := send(t, queueWatch(t, 80, 18, 3), keyPress("tab")); m.(watchModel).focusQueue {
		t.Fatal("tab skips a queue the layout left out")
	}
	if m := send(t, queueWatch(t, 160, 40, 3), keyPress("tab")); m.(watchModel).focusQueue || !m.(watchModel).focusLogs {
		t.Fatal("tab skips a queue with nothing to scroll")
	}
}

func TestWatchHeaderShouldShowTheSlotsAndWarnAtTheMaximum(t *testing.T) {
	for _, c := range []struct {
		width int
		want  string
	}{{160, "slots 2/3 · 1 queue"}, {120, "slots 2/3 · 1 queue"}, {80, "2/3"}} {
		src := &fakeWatchSource{status: map[int64]RunStatus{}, slots: SlotsView{InUse: 2, Max: 3, Queue: true}}
		if head := strings.Split(stripANSI(startWatch(t, src, c.width).View()), "\n")[0]; !strings.Contains(head, c.want) {
			t.Fatalf("%d columns: the header misses %q: %q", c.width, c.want, head)
		}
	}
	m := watchModel{st: lineStyles(termenv.ANSI), data: watchData{slots: SlotsView{InUse: 3, Max: 3}}}
	if got := m.slots(true); got != m.st.warn.Render("slots 3/3") {
		t.Fatalf("every slot in use is a warning: %q", got)
	}
	m.data.slots.InUse = 1
	if got := m.slots(true); got != m.st.info.Render("slots 1/3") {
		t.Fatalf("a free slot is not: %q", got)
	}
}

func TestQueueLinesShouldNameTheRepositoryWhenTheQueueHasSeveral(t *testing.T) {
	items := []store.QueueItem{
		{ID: 1, Position: 1, Repo: "/src/app", Issue: 12},
		{ID: 2, Position: 2, Repo: "/src/api", Issue: 12},
	}
	m := watchModel{st: noColors(), data: watchData{queue: items}}
	if got := m.queueLines(60); len(got) != 1 || got[0] != " P:1 app#12   P:2 api#12 " {
		t.Fatalf("the same number of two repositories is told apart: %q", got)
	}
	m.data.queue = queueOf(12, 21)
	if got := m.queueLines(60); got[0] != " P:1 #12   P:2 #21 " {
		t.Fatalf("a queue of one repository keeps its cells short: %q", got)
	}
}
