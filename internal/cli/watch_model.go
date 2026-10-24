package cli

import (
	"context"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/store"
)

var watchKeys = struct {
	quit, up, down, first, last, source, pageUp, pageDown, follow, focus, zoom, details, back, expand, prevTab, nextTab, stop, remove, open, copyID, help key.Binding
}{
	quit:     key.NewBinding(key.WithKeys("q", "ctrl+c")),
	stop:     key.NewBinding(key.WithKeys("s")),
	remove:   key.NewBinding(key.WithKeys("d")),
	open:     key.NewBinding(key.WithKeys("o")),
	copyID:   key.NewBinding(key.WithKeys("y")),
	help:     key.NewBinding(key.WithKeys("?")),
	details:  key.NewBinding(key.WithKeys("enter")),
	expand:   key.NewBinding(key.WithKeys("e")),
	prevTab:  key.NewBinding(key.WithKeys("left", "h")),
	nextTab:  key.NewBinding(key.WithKeys("right", "l")),
	back:     key.NewBinding(key.WithKeys("esc")),
	source:   key.NewBinding(key.WithKeys("1", "2", "3", "4", "5", "6")),
	pageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+u")),
	pageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+d")),
	follow:   key.NewBinding(key.WithKeys("f")),
	focus:    key.NewBinding(key.WithKeys("tab")),
	zoom:     key.NewBinding(key.WithKeys("z")),
	up:       key.NewBinding(key.WithKeys("up", "k")),
	down:     key.NewBinding(key.WithKeys("down", "j")),
	first:    key.NewBinding(key.WithKeys("g", "home")),
	last:     key.NewBinding(key.WithKeys("G", "end")),
}

// watchData is one reload of everything the screen shows.
type watchData struct {
	active, done []RunView
	status       RunStatus // of statusOf, the run selected when the reload started
	statusOf     int64
	today        budget.Status
	daemon       bool
	queue        []store.QueueItem
	logs         []logLine    // new lines of logsOf, read by follower
	skipped      int          // lines of logsOf left unread before logs
	logsOf       int64        // the run selected when the reload started
	follower     *logFollower // the follower the lines come from
}

type watchDataMsg watchData

// watchErrMsg is a reload that failed: the screen keeps the last data and shows why.
type watchErrMsg struct{ err error }

// watchTickMsg asks for the next reload.
type watchTickMsg struct{}

// watchModel is the state of `watch`: it only reads, every reload runs in a tea.Cmd.
type watchModel struct {
	src           watchSource
	st            styles
	opts          watchOptions
	width, height int
	layout        watchLayout
	data          watchData
	selectedID    int64 // the selection follows the run, wherever it moves in the list
	selectedIdx   int
	loading       bool // a reload is running: ticks are skipped until it ends
	err           error
	logs          logPane
	focusLogs     bool // the keys that move the selection scroll the logs instead
	focusQueue    bool // the keys that move the selection scroll the queue instead
	queueScroll   int  // the first row of the queue grid shown
	zoomedQueue   bool // focusQueue before the zoom
	zoom          bool // the logs take the whole width
	zoomedFrom    bool // focusLogs before the zoom
	fullStatus    bool // the full status takes the place of the strip and the logs
	expanded      bool // the compact list shows the finished runs
	tab           miniTab
	dialog        watchDialog // over the screen: only its keys act
	toast         watchToast  // on the first line of the footer
	actingToast   watchToast  // the toast of the running action: it comes back after a quick one
	toasts        int         // toasts shown so far: the seq of the last one
	acting        bool        // a stop or a remove is running
}

// miniTab is the panel the mini layout shows.
type miniTab int

const (
	tabRuns miniTab = iota
	tabStatus
	tabLogs
)

func newWatchModel(src watchSource, st styles, opts watchOptions) watchModel {
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.after == nil {
		opts.after = tea.Tick
	}
	return watchModel{src: src, st: st, opts: opts, loading: true, tab: tabLogs} // Init starts the first reload
}

func (m watchModel) Init() tea.Cmd { return tea.Batch(m.reload(), m.tick()) }

func (m watchModel) tick() tea.Cmd {
	return tea.Tick(m.opts.interval, func(time.Time) tea.Msg { return watchTickMsg{} })
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	if v, ok := m.selected(); ok && v.terminal {
		m.expanded = true // a folded list would hide the selection
	}
	// The panels follow the window, the data and the keys: the logs scroll by what they show.
	m.logs.resize(m.logRows())
	if m.focusQueue && !m.queueScrolls() {
		m.focusQueue = false // nothing left to scroll, or no panel: the focus goes back to the runs
	}
	return m, cmd
}

func (m watchModel) update(msg tea.Msg) (watchModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout = layoutFor(m.width, m.height)
	case watchTickMsg:
		if m.loading {
			return m, m.tick()
		}
		m.loading = true
		return m, tea.Batch(m.reload(), m.tick())
	case watchErrMsg:
		m.loading, m.err = false, msg.err
	case watchDataMsg:
		m.loading, m.err = false, nil
		if msg.follower != nil && msg.follower == m.logs.fl && msg.logsOf == m.selectedID {
			m.logs.append(msg.logs, msg.skipped) // lines of a run no longer selected are dropped
		}
		m.data = cleanData(watchData(msg))
		m.selectedIdx = pick(m.runs(), m.selectedID, m.selectedIdx)
		m.selectedID = 0
		if runs := m.runs(); len(runs) > 0 {
			m.selectedID = runs[m.selectedIdx].ID
		}
		m.followSelection()
		if m.selectedID != m.data.statusOf {
			// The selection moved while this reload ran: its status is still to be read.
			m.loading = true
			return m, m.reload()
		}
	case watchActionMsg:
		return m.actionDone(msg)
	case watchQuickMsg:
		return m.result(watchActionMsg(msg))
	case watchToastMsg:
		m = m.expireToast(msg.seq)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m watchModel) key(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	runs := m.runs()
	idx := m.selectedIdx
	if m.dialog.kind != dialogNone {
		if msg.Type == tea.KeyCtrlC { // the way out, whatever is open
			return m, tea.Quit
		}
		return m.dialogKey(msg)
	}
	switch {
	case key.Matches(msg, watchKeys.quit):
		return m, tea.Quit
	case key.Matches(msg, watchKeys.stop):
		return m.openDialog(dialogStop), nil
	case key.Matches(msg, watchKeys.remove):
		return m.openDialog(dialogRemove), nil
	case key.Matches(msg, watchKeys.open):
		return m.openPR()
	case key.Matches(msg, watchKeys.copyID):
		return m.copyID()
	case key.Matches(msg, watchKeys.help):
		m.dialog = watchDialog{kind: dialogHelp}
		return m, nil
	case key.Matches(msg, watchKeys.source):
		m.logs.setFilter(int(msg.Runes[0] - '1'))
		return m, nil
	case key.Matches(msg, watchKeys.pageUp):
		m.logs.pageUp()
		return m, nil
	case key.Matches(msg, watchKeys.pageDown):
		m.logs.pageDown()
		return m, nil
	case key.Matches(msg, watchKeys.follow):
		m.logs.toggleFollow()
		return m, nil
	case key.Matches(msg, watchKeys.focus):
		if !m.zoom { // the list is hidden in full screen
			m = m.nextFocus()
		}
		return m, nil
	case key.Matches(msg, watchKeys.details):
		if m.layout == layoutMedium || m.layout == layoutCompact { // the others show it already
			m.fullStatus = !m.fullStatus
		}
		return m, nil
	case key.Matches(msg, watchKeys.back):
		if m.layout == layoutMedium || m.layout == layoutCompact {
			m.fullStatus = false
		}
		return m, nil
	case key.Matches(msg, watchKeys.expand):
		if m.layout == layoutCompact { // the others always list the finished runs
			m.expanded = !m.expanded
		}
		return m, nil
	case key.Matches(msg, watchKeys.prevTab), key.Matches(msg, watchKeys.nextTab):
		if m.layout == layoutMini {
			step := map[bool]miniTab{true: -1, false: 1}[key.Matches(msg, watchKeys.prevTab)]
			m.tab = min(max(m.tab+step, tabRuns), tabLogs)
		}
		return m, nil
	case key.Matches(msg, watchKeys.zoom):
		if m.zoom = !m.zoom; m.zoom {
			m.zoomedFrom, m.zoomedQueue = m.focusLogs, m.focusQueue
			m.focusLogs, m.focusQueue = true, false
		} else {
			m.focusLogs, m.focusQueue = m.zoomedFrom, m.zoomedQueue
		}
		return m, nil
	case m.focusLogs && (m.layout != layoutMini || m.tab == tabLogs): // the other tabs move the selection
		return m.scrollLogs(msg), nil
	case m.focusQueue:
		return m.scrollQueue(msg), nil
	case key.Matches(msg, watchKeys.up):
		idx--
	case key.Matches(msg, watchKeys.down):
		idx++
	case key.Matches(msg, watchKeys.first):
		idx = 0
	case key.Matches(msg, watchKeys.last):
		idx = len(runs) - 1
	}
	idx = max(min(idx, len(runs)-1), 0)
	if len(runs) == 0 || runs[idx].ID == m.selectedID {
		return m, nil
	}
	m.selectedIdx, m.selectedID = idx, runs[idx].ID
	m.followSelection()
	if m.loading {
		return m, nil // the running reload reads the new selection's status when it ends
	}
	// The new selection's spend comes from its status: read it now, not on the next tick.
	m.loading = true
	return m, m.reload()
}

// nextFocus moves the focus from the runs to the queue, when it has rows to scroll, to the logs
// and back to the runs.
func (m watchModel) nextFocus() watchModel {
	switch {
	case m.focusLogs:
		m.focusLogs = false
	case m.focusQueue:
		m.focusQueue, m.focusLogs = false, true
	case m.queueScrolls():
		m.focusQueue = true
	default:
		m.focusLogs = true
	}
	return m
}

// queueScrolls reports whether the layout shows the queue panel and its grid is taller than it.
func (m watchModel) queueScrolls() bool {
	f := m.frame()
	return f.panels.queue.h > 0 && len(f.queue) > queueRows
}

// scrollQueue moves the queue grid a row at a time with the keys that move the selection.
func (m watchModel) scrollQueue(msg tea.KeyMsg) watchModel {
	rows := len(m.frame().queue)
	top := m.queueTop(rows)
	switch {
	case key.Matches(msg, watchKeys.up):
		top--
	case key.Matches(msg, watchKeys.down):
		top++
	case key.Matches(msg, watchKeys.first):
		top = 0
	case key.Matches(msg, watchKeys.last):
		top = rows
	}
	m.queueScroll = max(min(top, rows-queueRows), 0)
	return m
}

// scrollLogs moves the logs with the keys that move the selection when the list has the focus.
func (m watchModel) scrollLogs(msg tea.KeyMsg) watchModel {
	switch {
	case key.Matches(msg, watchKeys.up):
		m.logs.lineUp()
	case key.Matches(msg, watchKeys.down):
		m.logs.lineDown()
	case key.Matches(msg, watchKeys.first):
		m.logs.top()
	case key.Matches(msg, watchKeys.last):
		m.logs.follow()
	}
	return m
}

// followSelection points the logs panel at the selected run when it is another one.
func (m *watchModel) followSelection() {
	if m.logs.run == m.selectedID {
		return
	}
	dir := ""
	if v, ok := m.selected(); ok && v.RunDir != "" {
		dir = filepath.Join(v.RunDir, "logs")
	}
	m.logs.reset(m.selectedID, dir)
}

// logRows is how many log lines fit in the logs panel of the layout.
func (m watchModel) logRows() int {
	_, rows := paneRows(max(m.frame().panels.logs.h-2, 0), m.logLook()) // borders
	return rows
}

// logLook is how the logs panel fits the layout.
func (m watchModel) logLook() logLook {
	switch m.layout {
	case layoutCompact:
		return logsShort
	case layoutMini:
		return logsLetters
	}
	return logsFull
}

// runs is the list in screen order: the active runs, then the finished ones.
func (m watchModel) runs() []RunView {
	return append(append([]RunView{}, m.data.active...), m.data.done...)
}

// selected is the run the status panel shows.
func (m watchModel) selected() (RunView, bool) {
	runs := m.runs()
	if len(runs) == 0 {
		return RunView{}, false
	}
	return runs[m.selectedIdx], true
}

// pick is the index of the run with id, or the one at idx when it is gone, or the last one.
func pick(runs []RunView, id int64, idx int) int {
	for i, v := range runs {
		if v.ID == id {
			return i
		}
	}
	return max(min(idx, len(runs)-1), 0)
}

// reload reads the runs, the status of the selected one and its new log lines in a tea.Cmd,
// off the Update loop.
func (m watchModel) reload() tea.Cmd {
	src, limit, id, idx := m.src, m.opts.limit, m.selectedID, m.selectedIdx
	fl, logsOf := m.logs.fl, m.logs.run
	// An ended run writes nothing more: its unterminated last lines are complete.
	v, _ := m.selected()
	flush := v.ID == logsOf && v.terminal
	return func() tea.Msg {
		ctx := context.Background()
		active, done, err := src.Runs(ctx, limit)
		if err != nil {
			return watchErrMsg{err}
		}
		d := watchData{active: active, done: done, daemon: src.DaemonRunning()}
		if d.today, err = src.Today(ctx); err != nil {
			return watchErrMsg{err}
		}
		if d.queue, err = src.Queue(ctx); err != nil {
			return watchErrMsg{err}
		}
		if runs := append(append([]RunView{}, active...), done...); len(runs) > 0 {
			d.statusOf = runs[pick(runs, id, idx)].ID
			if d.status, err = src.Status(ctx, d.statusOf); err != nil {
				return watchErrMsg{err}
			}
		}
		// Read last: the lines read are lost if the reload fails after them.
		d.logsOf, d.follower = logsOf, fl
		if fl != nil {
			if d.skipped, err = skipBacklog(fl); err != nil {
				return watchErrMsg{err}
			}
			if d.logs, err = fl.read(flush); err != nil {
				return watchErrMsg{err}
			}
			capPartial(fl)
		}
		return watchDataMsg(d)
	}
}
