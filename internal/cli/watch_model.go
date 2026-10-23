package cli

import (
	"context"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/budget"
)

var watchKeys = struct {
	quit key.Binding
}{
	quit: key.NewBinding(key.WithKeys("q", "ctrl+c")),
}

// watchData is one reload of everything the screen shows.
type watchData struct {
	active, done []RunView
	status       RunStatus // of the run selected when the reload started
	today        budget.Status
	daemon       bool
}

type watchDataMsg watchData

// watchModel is the state of `watch`: it only reads, every reload runs in a tea.Cmd.
type watchModel struct {
	src           watchSource
	st            styles
	opts          watchOptions
	width, height int
	data          watchData
	selectedID    int64 // the selection follows the run, wherever it moves in the list
	selectedIdx   int
}

func newWatchModel(src watchSource, st styles, opts watchOptions) watchModel {
	return watchModel{src: src, st: st, opts: opts}
}

func (m watchModel) Init() tea.Cmd { return m.reload() }

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case watchDataMsg:
		m.data = watchData(msg)
		m.selectedIdx = pick(m.runs(), m.selectedID, m.selectedIdx)
		m.selectedID = 0
		if runs := m.runs(); len(runs) > 0 {
			m.selectedID = runs[m.selectedIdx].ID
		}
	case tea.KeyMsg:
		if key.Matches(msg, watchKeys.quit) {
			return m, tea.Quit
		}
	}
	return m, nil
}

// runs is the list in screen order: the active runs, then the finished ones.
func (m watchModel) runs() []RunView {
	return append(append([]RunView{}, m.data.active...), m.data.done...)
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

// reload reads the runs and the status of the selected one in a tea.Cmd, off the Update loop.
func (m watchModel) reload() tea.Cmd {
	src, limit := m.src, m.opts.limit
	return func() tea.Msg {
		active, done, _ := src.Runs(context.Background(), limit)
		return watchDataMsg{active: active, done: done}
	}
}
