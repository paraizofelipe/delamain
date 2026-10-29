package cli

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

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
