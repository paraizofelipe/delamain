package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/paraizo/delamain/internal/forge"
)

// prListFor is how long the list of the open PRs may take: a forge on a dead network would hang.
const prListFor = 30 * time.Second

// prListMax is how many open PRs r lists, newest first.
const prListMax = 50

// watchPRsMsg is how the load of the open PRs ended: the PRs, or why there are none to show.
type watchPRsMsg struct {
	id  uint64 // the load it answers
	prs []forge.PRDetail
	err error
}

// loadPRs lists the open PRs of the repository picked, off Update: it is answered by a watchPRsMsg.
func (m *watchModel) loadPRs() tea.Cmd {
	id := repoCheckSeq.Add(1)
	p := &m.dialog.issue
	p.loading, p.loadID, p.prs, p.prCursor = true, id, nil, 0
	repo, open := p.repo, m.opts.prForge
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), prListFor)
		defer cancel()
		f, err := open(ctx, repo)
		if err != nil {
			return watchPRsMsg{id: id, err: err}
		}
		prs, err := f.ListOpenPRs(ctx, prListMax)
		return watchPRsMsg{id: id, prs: prs, err: err}
	}
}

// prsLoaded shows the open PRs of the load awaited.
func (m watchModel) prsLoaded(msg watchPRsMsg) watchModel {
	p := &m.dialog.issue
	if m.dialog.kind != dialogStartReview || !p.loading || msg.id != p.loadID {
		return m // the dialog closed, or the repository was replaced, while the PRs loaded
	}
	p.loading, p.prs = false, msg.prs
	return m
}

// prLines are the step of the dialog of r that lists the open PRs.
func (m watchModel) prLines(d *dialogWriter) {
	st, p := m.st, m.dialog.issue
	d.text(st.none.Bold(true), fmt.Sprintf("Which PR in %s?", filepath.Base(p.repo)))
	d.blank()
	d.row("repo", paint(st.muted, p.repo))
	d.blank()
	if p.loading {
		d.text(st.muted, "loading PRs…")
	}
	for i, pr := range p.prs {
		mark := "  "
		if i == p.prCursor {
			mark = paint(st.info.Bold(true), "› ")
		}
		head := fmt.Sprintf("#%d ", pr.Number)
		tail := " @" + clean(pr.Author)
		if pr.Draft {
			tail += " draft"
		}
		room := max(d.inner-lipgloss.Width(mark+head+tail), 1)
		d.styled(mark + paint(st.info, head) + ansi.Truncate(clean(pr.Title), room, "…") + paint(st.muted, tail))
	}
	m.pickerError(d)
	d.buttons([2]string{"review", "enter"}, [2]string{"back", "esc"})
}
