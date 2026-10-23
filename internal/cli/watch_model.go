package cli

import (
	"context"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/budget"
)

var watchKeys = struct {
	quit, up, down, first, last, source, pageUp, pageDown, follow, focus, zoom key.Binding
}{
	quit:     key.NewBinding(key.WithKeys("q", "ctrl+c")),
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
	zoom          bool // the logs take the whole width
	zoomedFrom    bool // focusLogs before the zoom
}

func newWatchModel(src watchSource, st styles, opts watchOptions) watchModel {
	if opts.now == nil {
		opts.now = time.Now
	}
	return watchModel{src: src, st: st, opts: opts, loading: true} // Init starts the first reload
}

func (m watchModel) Init() tea.Cmd { return tea.Batch(m.reload(), m.tick()) }

func (m watchModel) tick() tea.Cmd {
	return tea.Tick(m.opts.interval, func(time.Time) tea.Msg { return watchTickMsg{} })
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	// The panels follow the window, the data and the keys: the logs scroll by what they show.
	m.logs.resize(m.logRows())
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
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m watchModel) key(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	runs := m.runs()
	idx := m.selectedIdx
	switch {
	case key.Matches(msg, watchKeys.quit):
		return m, tea.Quit
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
			m.focusLogs = !m.focusLogs
		}
		return m, nil
	case key.Matches(msg, watchKeys.zoom):
		if m.zoom = !m.zoom; m.zoom {
			m.zoomedFrom, m.focusLogs = m.focusLogs, true
		} else {
			m.focusLogs = m.zoomedFrom
		}
		return m, nil
	case m.focusLogs:
		return m.scrollLogs(msg), nil
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
	_, rows := paneRows(max(m.frame().panels.logs.h-2, 0)) // borders
	return rows
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
