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
		{"an empty list keeps room for a few rows", 0, 5, 33},
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

func TestPanelsForShouldPutTheQueueBetweenTheRunsAndTheStatus(t *testing.T) {
	// 140×40 with a footer line: the body is 38 lines, the list 8 and the rest 30.
	p := panelsFor(layoutWide, 140, 40, panelNeeds{footer: 1, list: 6, status: 16, queue: queueRows})
	if want := (rect{0, 9, 56, 5}); p.queue != want || p.status != (rect{0, 14, 56, 25}) || p.logs != (rect{56, 9, 84, 30}) {
		t.Errorf("wide: the queue opens the left column, below the runs and left of the logs: %+v, want queue %+v", p, want)
	}
	p = panelsFor(layoutMedium, 120, 30, panelNeeds{footer: 1, list: 6, strip: 3, queue: queueRows})
	if p.queue != (rect{45, 1, 75, 5}) || p.strip.y != 6 || p.logs.y != 11 || p.logs.h != 18 {
		t.Errorf("medium: the queue opens the right column, above the strip: %+v", p)
	}
	p = panelsFor(layoutMedium, 120, 30, panelNeeds{footer: 1, list: 6, status: 20, queue: queueRows, full: true})
	if p.queue != (rect{45, 1, 75, 5}) || p.status != (rect{45, 6, 75, 23}) {
		t.Errorf("medium full: the queue stays above the status: %+v", p)
	}
	p = panelsFor(layoutMedium, 120, 30, panelNeeds{footer: 1, list: 6, status: 24, queue: queueRows, full: true})
	if p.queue != (rect{}) || p.status != (rect{45, 1, 75, 28}) {
		t.Errorf("medium full: the full status goes before the queue: %+v", p)
	}
	p = panelsFor(layoutCompact, 80, 30, panelNeeds{footer: 1, list: 6, strip: 3, queue: queueRows})
	if p.list.h != 8 || p.queue.y != p.list.y+p.list.h || p.queue.h != 5 || p.strip.y != p.queue.y+p.queue.h ||
		p.logs.y != p.strip.y+p.strip.h || p.logs.h < minLogs {
		t.Errorf("compact: list, queue, strip, logs: %+v", p)
	}
	p = panelsFor(layoutCompact, 80, 18, panelNeeds{footer: 1, list: 6, strip: 3, queue: queueRows})
	if p.list.h != 6 || p.queue != (rect{}) {
		t.Errorf("compact: the runs go ahead of the queue, which is left out when it does not fit whole: %+v", p)
	}
	if p := panelsFor(layoutMini, 60, 20, panelNeeds{footer: 1, queue: queueRows, tab: tabStatus}); p.queue != (rect{}) {
		t.Errorf("mini: no queue panel: %+v", p.queue)
	}
}
