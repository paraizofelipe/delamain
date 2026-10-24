package cli

import "testing"

func TestLayoutForShouldPickTheLayoutByWidthAndHeight(t *testing.T) {
	for _, c := range []struct {
		width, height int
		want          watchLayout
	}{
		{200, 50, layoutWide},
		{140, 32, layoutWide},
		{139, 40, layoutMedium},
		{140, 31, layoutMedium},
		{100, 24, layoutMedium},
		{99, 40, layoutCompact},
		{100, 23, layoutCompact},
		{80, 27, layoutCompact}, // tmux display-popup -w 50% -h 60% on a 160×45 terminal
		{70, 18, layoutCompact},
		{69, 30, layoutMini},
		{70, 17, layoutMini},
		{40, 12, layoutMini},
		{39, 12, layoutTooSmall},
		{40, 11, layoutTooSmall},
		{0, 0, layoutTooSmall},
	} {
		if got := layoutFor(c.width, c.height); got != c.want {
			t.Errorf("layoutFor(%d, %d) = %v, want %v", c.width, c.height, got, c.want)
		}
	}
}

func TestPanelsForWideShouldPutTheListOnTopAndCapItAtHalfTheBody(t *testing.T) {
	// 140×40 with a footer line: the body is 38 lines and the left column 56 columns.
	for _, c := range []struct {
		name       string
		list       int
		wantList   int // height of the runs panel
		wantBottom int // height of the status panel and of the logs
	}{
		{"a list that fits keeps its rows", 6, 8, 30},
		{"a list longer than half the body stops there", 40, 19, 19},
		{"an empty list still shows its panel", 0, 2, 36},
	} {
		p := panelsFor(layoutWide, 140, 40, panelNeeds{footer: 1, list: c.list, status: 16})
		want := watchPanels{
			list:   rect{0, 1, 140, c.wantList},
			status: rect{0, 1 + c.wantList, 56, c.wantBottom},
			logs:   rect{56, 1 + c.wantList, 84, c.wantBottom},
		}
		if p != want {
			t.Errorf("%s: panelsFor gave %+v, want %+v", c.name, p, want)
		}
	}
}

func TestPanelsForWideShouldHideEveryPanelOnABodyWithNoRoom(t *testing.T) {
	p := panelsFor(layoutWide, 140, 1, panelNeeds{footer: 1, list: 30, status: 16})
	for _, r := range []rect{p.list, p.status, p.logs} {
		if r.h != 0 { // a zero height hides the panel
			t.Errorf("panelsFor gave %+v, want every panel hidden: %+v", r, p)
		}
	}
}

func TestPanelsForShouldPutTheQueueRightBelowTheStatus(t *testing.T) {
	// 140×40 with a footer line: the body is 38 lines, the list 8 and the rest 30.
	p := panelsFor(layoutWide, 140, 40, panelNeeds{footer: 1, list: 6, status: 16, queue: 2})
	if want := (rect{0, 35, 56, 4}); p.queue != want || p.status != (rect{0, 9, 56, 26}) || p.logs != (rect{56, 9, 84, 30}) {
		t.Errorf("wide: the queue takes the foot of the status column: %+v, want queue %+v", p, want)
	}
	p = panelsFor(layoutWide, 140, 40, panelNeeds{footer: 1, list: 6, status: 16, queue: 40})
	if p.queue.h != 10 {
		t.Errorf("wide: a long queue stops at a third of the column: %+v", p.queue)
	}
	p = panelsFor(layoutMedium, 120, 30, panelNeeds{footer: 1, list: 6, strip: 3, queue: 1})
	if p.queue != (rect{45, 6, 75, 3}) || p.logs.y != 9 || p.logs.h != 20 {
		t.Errorf("medium: the queue sits between the strip and the logs: %+v", p)
	}
	p = panelsFor(layoutCompact, 80, 24, panelNeeds{footer: 1, list: 6, strip: 3, queue: 1})
	if p.queue.y != p.strip.y+p.strip.h || p.logs.y != p.queue.y+p.queue.h || p.logs.h < minLogs {
		t.Errorf("compact: strip, queue, logs: %+v", p)
	}
	if p := panelsFor(layoutMini, 60, 20, panelNeeds{footer: 1, queue: 2, tab: tabStatus}); p.queue != (rect{}) {
		t.Errorf("mini: no queue panel: %+v", p.queue)
	}
}
