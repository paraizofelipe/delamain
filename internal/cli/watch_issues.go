package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/paraizo/delamain/internal/forge"
)

// issueListMax is how many open issues i lists, newest first.
const issueListMax = 50

// watchIssuesMsg is how the load of the open issues ended: the issues, or why there are none to
// show.
type watchIssuesMsg struct {
	id     uint64 // the load it answers
	top    string // the main worktree of the repository, empty when git could not resolve it
	issues []forge.Issue
	err    error
}

// loadIssues lists the open issues of the repository picked, off Update: it is answered by a
// watchIssuesMsg.
func (m *watchModel) loadIssues() tea.Cmd {
	id := repoCheckSeq.Add(1)
	p := &m.dialog.issue
	p.loading, p.loadID, p.issues, p.listCursor = true, id, nil, 0
	repo, open := p.repo, m.opts.prForge
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), prListFor)
		defer cancel()
		top, f, err := pickedForge(ctx, open, repo)
		if err != nil {
			return watchIssuesMsg{id: id, top: top, err: err}
		}
		issues, err := f.ListOpenIssues(ctx, issueListMax)
		return watchIssuesMsg{id: id, top: top, issues: issues, err: err}
	}
}

// issuesLoaded shows the open issues of the load awaited, or why there are none.
func (m watchModel) issuesLoaded(msg watchIssuesMsg) watchModel {
	p := &m.dialog.issue
	if m.dialog.kind != dialogStartIssue || !p.loading || msg.id != p.loadID {
		return m // the dialog closed, or the repository was replaced, while the issues loaded
	}
	p.loading, p.issues, p.top = false, msg.issues, msg.top
	if msg.err != nil {
		p.err = clean(msg.err.Error())
	}
	return m
}

// issueListKey handles a key of the list of issues: enter picks the issue under the cursor, esc
// goes back to the repository.
func (m watchModel) issueListKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	p := &m.dialog.issue
	last := max(len(p.issues)-1, 0)
	switch {
	case key.Matches(msg, pickerKeys.back):
		p.issues = nil
		m.backToRepo()
	case key.Matches(msg, watchKeys.up):
		p.listCursor = max(p.listCursor-1, 0)
	case key.Matches(msg, watchKeys.down):
		p.listCursor = min(p.listCursor+1, last)
	case key.Matches(msg, watchKeys.first):
		p.listCursor = 0
	case key.Matches(msg, watchKeys.last):
		p.listCursor = last
	case key.Matches(msg, pickerKeys.next) && len(p.issues) > 0:
		p.step, p.number = pickConfirm, p.issues[p.listCursor].Number
		m.dialog.at = m.opts.now() // the enter of the list, held, must not answer the confirmation
	}
	return m, nil
}

// issueLines are the step of the dialog of i that lists the open issues.
func (m watchModel) issueLines(d *dialogWriter) {
	st, p := m.st, m.dialog.issue
	d.text(st.none.Bold(true), fmt.Sprintf("Which issue in %s?", filepath.Base(p.repo)))
	d.blank()
	d.row("repo", paint(st.muted, p.repo))
	d.blank()
	switch {
	case p.loading:
		d.text(st.muted, "loading issues…")
	case len(p.issues) == 0 && p.err == "":
		d.text(st.muted, "no open issues in "+filepath.Base(p.repo))
	}
	for i, issue := range p.issues {
		mark := "  "
		if i == p.listCursor {
			mark = paint(st.info.Bold(true), "› ")
		}
		head := fmt.Sprintf("#%d ", issue.Number)
		room := max(d.inner-lipgloss.Width(mark+head), 1)
		d.styled(mark + paint(st.info, head) + ansi.Truncate(clean(issue.Title), room, "…"))
	}
	m.pickerError(d)
	d.buttons([2]string{"next", "enter"}, [2]string{"back", "esc"})
}
