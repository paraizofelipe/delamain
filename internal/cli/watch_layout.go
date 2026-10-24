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
// status, status the whole of it, and queue the queue right below either one.
type watchPanels struct {
	tabs, list, strip, status, queue, logs rect
}

// panelNeeds is what the panels hold, in lines, and the state that shapes them.
type panelNeeds struct {
	footer int  // lines of the footer
	list   int  // lines of the list
	strip  int  // lines of the status strip
	status int  // lines of the status panel
	queue  int  // lines of the queue grid, fixed; 0 leaves the panel out
	zoom   bool // the logs take the whole body
	full   bool // the full status takes the place of the strip and the logs
	tab    miniTab
}

// leftWidth is the width of the status panel of the wide layout.
func leftWidth(width int) int { return min(max(width*40/100, 56), 72, width) }

// mediumLeft is the width of the left column of the medium layout.
func mediumLeft(width int) int { return min(max(width*38/100, 36), 48, width) }

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
	// The list takes the top of the screen, its rows up to half the body; the left column and
	// the logs share the rest of it, side by side. The queue opens the left column, in up to a
	// third of it, and the status panel takes what is left below.
	listH := min(2+max(n.list, minListRows), body/2)
	rest := body - listH
	queue := queueHeight(n, rest/3)
	return watchPanels{
		list:   rect{0, 1, width, listH},
		queue:  queueRect(0, 1+listH, left, queue),
		status: rect{0, 1 + listH + queue, left, rest - queue},
		logs:   rect{left, 1 + listH, width - left, rest},
	}
}

// queueHeight is the height of the queue panel, its grid and borders, when room holds it; a
// panel with no room for its whole grid is left out.
func queueHeight(n panelNeeds, room int) int {
	if n.queue == 0 || room < 2+n.queue {
		return 0
	}
	return 2 + n.queue
}

// queueRect is the queue panel's rectangle; a zero height is the zero rect, as for every panel
// a layout leaves out.
func queueRect(x, y, w, h int) rect {
	if h == 0 {
		return rect{}
	}
	return rect{x, y, w, h}
}

// minFullLogs is the height of the smallest logs panel that shows its chips and separators too.
const minFullLogs = minLogs + 2

// minListRows is the least the runs panel holds, with no run or a few, before its borders.
const minListRows = 3

// minLogs is the height of the smallest logs panel: borders, chips, a log line and its footer.
const minLogs = 5

// compactPanels stacks the list, the queue, the status strip and the logs on the whole width: the
// logs keep minLogs lines and the list its selected row, the strip takes what it needs of the
// rest, then the list, then the queue when it fits whole, and the logs get what is left. The
// full status replaces the strip and the logs.
func compactPanels(width, body int, n panelNeeds) watchPanels {
	if n.full { // the list shrinks to its selected row before the status does
		list := min(2+n.list, max(body-2-n.status, 3), body)
		queue := queueHeight(n, body-list-2-n.status) // the full status first: enter opens it to read it all
		return watchPanels{
			list:   rect{0, 1, width, list},
			queue:  queueRect(0, 1+list, width, queue),
			status: rect{0, 1 + list + queue, width, body - list - queue},
		}
	}
	strip := min(2+n.strip, max(body-minLogs-3, 0))
	list := min(2+max(n.list, minListRows), max(body-strip-minLogs, 0))
	queue := queueHeight(n, body-strip-list-minLogs)
	return watchPanels{
		list:  rect{0, 1, width, list},
		queue: queueRect(0, 1+list, width, queue),
		strip: rect{0, 1 + list + queue, width, strip},
		logs:  rect{0, 1 + list + queue + strip, width, body - list - queue - strip},
	}
}

// mediumPanels gives the list the whole left column, and the right one to the queue and the
// status strip on top of the logs, which keep minLogs lines; the full status replaces the strip
// and the logs.
func mediumPanels(width, body int, n panelNeeds) watchPanels {
	left := mediumLeft(width)
	p := watchPanels{list: rect{0, 1, left, body}}
	if n.full {
		queue := queueHeight(n, body-2-n.status) // the full status first: enter opens it to read it all
		p.queue, p.status = queueRect(left, 1, width-left, queue), rect{left, 1 + queue, width - left, body - queue}
		return p
	}
	strip := min(2+n.strip, max(body-minLogs, 0))
	queue := queueHeight(n, body-strip-minFullLogs)
	p.queue, p.strip = queueRect(left, 1, width-left, queue), rect{left, 1 + queue, width - left, strip}
	p.logs = rect{left, 1 + queue + strip, width - left, body - queue - strip}
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
