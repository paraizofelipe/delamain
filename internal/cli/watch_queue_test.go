package cli

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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

func TestQueueLinesShouldCountTheItemsPastTheLastRow(t *testing.T) {
	var issues []int
	for n := 1; n <= 30; n++ {
		issues = append(issues, 100+n)
	}
	m := watchModel{st: noColors(), data: watchData{queue: queueOf(issues...)}}
	got := m.queueLines(24) // two cells a row
	if len(got) != maxQueueRows {
		t.Fatalf("the grid stops at %d rows: %q", maxQueueRows, got)
	}
	last := got[len(got)-1]
	if !strings.HasSuffix(last, " +23") || !strings.Contains(last, "P:7 #107") || lipgloss.Width(last) > 24 {
		t.Fatalf("the last row ends with the count of what did not fit: %q", last)
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

func TestWatchShouldShowTheQueueBelowTheStatus(t *testing.T) {
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
		if status < 0 || queue <= status {
			t.Fatalf("%d columns: the queue is below the status:\n%s", width, view)
		}
	}
}
