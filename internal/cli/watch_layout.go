package cli

import "sort"

// watchLayout is how `watch` arranges its panels for the size of the terminal.
type watchLayout int

const (
	layoutWide watchLayout = iota
	layoutMedium
	layoutCompact
	layoutMini
	layoutTooSmall
)

func (l watchLayout) String() string {
	return [...]string{"wide", "medium", "compact", "mini", "tooSmall"}[l]
}

// layoutFor picks the layout of a terminal width × height cells big: the first one it fits.
func layoutFor(width, height int) watchLayout {
	switch {
	case width >= 140 && height >= 32:
		return layoutWide
	case width >= 100 && height >= 24:
		return layoutMedium
	case width >= 70 && height >= 18:
		return layoutCompact
	case width >= 40 && height >= 12:
		return layoutMini
	}
	return layoutTooSmall
}

// rect is a panel's place on the screen; a zero height hides it.
type rect struct{ x, y, w, h int }

// watchPanels are the rectangles of the panels of a layout: strip is the 3 line summary of the
// status, status the whole of it.
type watchPanels struct {
	tabs, list, strip, status, logs rect
}

// panelNeeds is what the panels hold, in lines, and the state that shapes them.
type panelNeeds struct {
	footer int  // lines of the footer
	list   int  // lines of the list
	strip  int  // lines of the status strip
	status int  // lines of the status panel
	zoom   bool // the logs take the whole body
	full   bool // the full status takes the place of the strip and the logs
	tab    miniTab
}

// leftWidth is the width of the status panel of the wide layout.
func leftWidth(width int) int { return min(max(width*40/100, 56), 72, width) }

// mediumLeft is the width of the left column of the medium layout.
func mediumLeft(width int) int { return min(max(width*38/100, 36), 48, width) }

// listRuns is how many runs the list of the wide layout always has room for, active and
// finished; the ones that do not fit are a scroll away.
const listRuns = 12

// listRows are the lines that list holds: the column header, the ACTIVE and FINISHED headings
// and a row per run.
const listRows = 3 + listRuns

// panelsFor places the panels of layout l on a width × height screen: the header takes the
// first line and the footer the last n.footer ones.
func panelsFor(l watchLayout, width, height int, n panelNeeds) watchPanels {
	body := max(height-1-n.footer, 0)
	switch l {
	case layoutTooSmall:
		return watchPanels{}
	case layoutMini: // the tabs line, then the panel of the tab on the rest
		p, tab := watchPanels{tabs: rect{0, 1, width, min(body, 1)}}, rect{0, 2, width, max(body-1, 0)}
		switch n.tab {
		case tabRuns:
			p.list = tab
		case tabStatus:
			p.status = tab
		default:
			p.logs = tab
		}
		return p
	}
	if n.zoom {
		return watchPanels{logs: rect{0, 1, width, body}}
	}
	switch l {
	case layoutCompact:
		return compactPanels(width, body, n)
	case layoutMedium:
		return mediumPanels(width, body, n)
	}
	left := leftWidth(width)
	// The list takes the top of the screen, with room for listRuns rows however many runs there
	// are, so removing one never moves what is under it; the status panel and the logs share the
	// rest of the body, side by side. A body too short to hold both keeps minLogs lines for them.
	listH := min(2+listRows, max(body-minLogs, 0))
	return watchPanels{
		list:   rect{0, 1, width, listH},
		status: rect{0, 1 + listH, left, body - listH},
		logs:   rect{left, 1 + listH, width - left, body - listH},
	}
}

// minLogs is the height of the smallest logs panel: borders, chips, a log line and its footer.
const minLogs = 5

// compactPanels stacks the list, the status strip and the logs on the whole width: the logs keep
// minLogs lines and the list its selected row, the strip takes what it needs of the rest, then
// the list, and the logs get what is left. The full status replaces the strip and the logs.
func compactPanels(width, body int, n panelNeeds) watchPanels {
	if n.full { // the list shrinks to its selected row before the status does
		list := min(2+n.list, max(body-2-n.status, 3), body)
		return watchPanels{list: rect{0, 1, width, list}, status: rect{0, 1 + list, width, body - list}}
	}
	strip := min(2+n.strip, max(body-minLogs-3, 0))
	list := min(2+n.list, max(body-strip-minLogs, 0))
	return watchPanels{
		list:  rect{0, 1, width, list},
		strip: rect{0, 1 + list, width, strip},
		logs:  rect{0, 1 + list + strip, width, body - list - strip},
	}
}

// mediumPanels gives the list the whole left column, and the right one to the status strip on
// top of the logs, which keep minLogs lines; the full status replaces the strip and the logs.
func mediumPanels(width, body int, n panelNeeds) watchPanels {
	left := mediumLeft(width)
	p := watchPanels{list: rect{0, 1, left, body}}
	if n.full {
		p.status = rect{left, 1, width - left, body}
		return p
	}
	strip := min(2+n.strip, max(body-minLogs, 0))
	p.strip, p.logs = rect{left, 1, width - left, strip}, rect{left, 1 + strip, width - left, body - strip}
	return p
}

// placed is a panel drawn in its rectangle.
type placed struct {
	r     rect
	lines []string
}

// compose lays the panels out on rows lines from the second one, side by side by their x.
func compose(rows int, parts ...placed) []string {
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].r.x < parts[j].r.x })
	out := make([]string, rows)
	for i := range out {
		y := 1 + i
		for _, p := range parts {
			if y >= p.r.y && y < p.r.y+p.r.h && y-p.r.y < len(p.lines) {
				out[i] += p.lines[y-p.r.y]
			}
		}
	}
	return out
}
