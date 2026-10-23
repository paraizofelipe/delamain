package cli

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

var watchKeys = struct {
	quit key.Binding
}{
	quit: key.NewBinding(key.WithKeys("q", "ctrl+c")),
}

// watchModel is the state of `watch`: it only reads, every reload runs in a tea.Cmd.
type watchModel struct {
	src  watchSource
	st   styles
	opts watchOptions
}

func newWatchModel(src watchSource, st styles, opts watchOptions) watchModel {
	return watchModel{src: src, st: st, opts: opts}
}

func (m watchModel) Init() tea.Cmd { return nil }

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok && key.Matches(msg, watchKeys.quit) {
		return m, tea.Quit
	}
	return m, nil
}

func (m watchModel) View() string { return "" }
