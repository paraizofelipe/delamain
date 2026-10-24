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

func TestPanelsForWideShouldPutTheListOnTopWithAFixedHeight(t *testing.T) {
	// 140×40 with a footer line: the body is 38 lines and the left column 56 columns. The list
	// holds 12 runs whatever it has to show, so removing one never moves the panels under it.
	for _, c := range []struct {
		name string
		list int
	}{
		{"a list of a few runs", 6},
		{"a list of more runs than it shows", 40},
		{"an empty list", 0},
	} {
		p := panelsFor(layoutWide, 140, 40, panelNeeds{footer: 1, list: c.list, status: 16})
		want := watchPanels{
			list:   rect{0, 1, 140, 17},
			status: rect{0, 18, 56, 21},
			logs:   rect{56, 18, 84, 21},
		}
		if p != want {
			t.Errorf("%s: panelsFor gave %+v, want %+v", c.name, p, want)
		}
	}
}

func TestPanelsForWideShouldShrinkTheListOnABodyTooShortForIt(t *testing.T) {
	// 140×20 with a footer line: 18 lines of body. No wide terminal is that short, but the cap
	// is what keeps minLogs lines for the status panel and the logs if one ever is.
	p := panelsFor(layoutWide, 140, 20, panelNeeds{footer: 1, list: 30, status: 16})
	want := watchPanels{
		list:   rect{0, 1, 140, 13},
		status: rect{0, 14, 56, 5},
		logs:   rect{56, 14, 84, 5},
	}
	if p != want {
		t.Errorf("panelsFor gave %+v, want %+v", p, want)
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
