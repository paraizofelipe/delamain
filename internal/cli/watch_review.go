package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/store"
)

// prListFor is how long the list of the open PRs may take: a forge on a dead network would hang.
const prListFor = 30 * time.Second

// prListMax is how many open PRs r lists, newest first.
const prListMax = 50

// watchPRsMsg is how the load of the open PRs ended: the PRs, or why there are none to show.
type watchPRsMsg struct {
	id  uint64 // the load it answers
	top string // the main worktree of the repository, empty when git could not resolve it
	prs []forge.PRDetail
	err error
}

// loadPRs lists the open PRs of the repository picked, off Update: it is answered by a watchPRsMsg.
func (m *watchModel) loadPRs() tea.Cmd {
	id := repoCheckSeq.Add(1)
	p := &m.dialog.issue
	p.loading, p.loadID, p.prs, p.listCursor = true, id, nil, 0
	repo, open := p.repo, m.opts.prForge
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), prListFor)
		defer cancel()
		top, f, err := pickedForge(ctx, open, repo)
		if err != nil {
			return watchPRsMsg{id: id, top: top, err: err}
		}
		prs, err := f.ListOpenPRs(ctx, prListMax)
		return watchPRsMsg{id: id, top: top, prs: prs, err: err}
	}
}

// pickedForge is the forge of repo, opened by open, and its main worktree: delamain run and
// delamain review record their runs under the main worktree, whichever subdirectory or linked
// worktree was picked, and the active runs are matched against that path. top is empty when git
// could not resolve it.
func pickedForge(ctx context.Context, open func(context.Context, string) (forge.Forge, error), repo string) (top string, f forge.Forge, err error) {
	top, _ = gitx.New().MainWorktree(ctx, repo)
	f, err = open(ctx, repo)
	switch {
	case err != nil:
		return top, nil, err
	case f == nil: // as delamain run and review, a repository without origin has nothing to list
		return top, nil, fmt.Errorf("%s has no GitHub or GitLab origin", repo)
	}
	return top, f, nil
}

// prsLoaded shows the open PRs of the load awaited, or why there are none.
func (m watchModel) prsLoaded(msg watchPRsMsg) watchModel {
	p := &m.dialog.issue
	if m.dialog.kind != dialogStartReview || !p.loading || msg.id != p.loadID {
		return m // the dialog closed, or the repository was replaced, while the PRs loaded
	}
	p.loading, p.prs, p.top = false, msg.prs, msg.top
	if msg.err != nil {
		p.err = clean(msg.err.Error())
	}
	return m
}

// prKey handles a key of the list of PRs: enter picks the PR under the cursor, esc goes back to
// the repository.
func (m watchModel) prKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	p := &m.dialog.issue
	last := max(len(p.prs)-1, 0)
	switch {
	case key.Matches(msg, pickerKeys.back):
		p.prs = nil
		m.backToRepo()
	case key.Matches(msg, watchKeys.up):
		p.listCursor = max(p.listCursor-1, 0)
	case key.Matches(msg, watchKeys.down):
		p.listCursor = min(p.listCursor+1, last)
	case key.Matches(msg, watchKeys.first):
		p.listCursor = 0
	case key.Matches(msg, watchKeys.last):
		p.listCursor = last
	case key.Matches(msg, pickerKeys.next) && len(p.prs) > 0:
		n := p.prs[p.listCursor].Number
		if m.activeRun(p.runRepo(), n) {
			return m.showToast(m.st.warn, fmt.Sprintf("a run is already active for PR %d", n))
		}
		p.step, p.number = pickConfirm, n
		m.dialog.at = m.opts.now() // the enter of the list, held, must not answer the confirmation
	}
	return m, nil
}

// backToRepo leaves the list of issues or PRs for the repository it came from: the list of
// daemon.repos, or the path typed.
func (m *watchModel) backToRepo() {
	p := &m.dialog.issue
	p.step, p.err, p.loading = pickRepo, "", false
	if len(m.opts.repos) == 0 || p.cursor == len(m.opts.repos) {
		p.step, p.input = pickPath, newPickerInput(p.path)
	}
}

// activeRun reports whether the last reload holds an active run of issue n in repo, for the
// dialog of i, or an active review or address of PR n, for the dialog of r: a second one would be
// refused by the store.
func (m watchModel) activeRun(repo string, n int) bool {
	num := strconv.Itoa(n)
	for _, v := range m.data.active {
		if filepath.Clean(v.Repo) != filepath.Clean(repo) {
			continue
		}
		if m.dialog.kind == dialogStartIssue {
			if v.Kind == string(store.KindIssue) && v.Issue == "#"+num {
				return true
			}
			continue
		}
		if v.Kind == string(store.KindReview) && v.Issue == "pr:"+num || v.Kind == string(store.KindAddress) && v.Issue == "addr:"+num {
			return true
		}
	}
	return false
}

// reviewLines are the confirmation of the dialog of r.
func (m watchModel) reviewLines(d *dialogWriter) {
	st, p := m.st, m.dialog.issue
	title := clean(p.prs[p.listCursor].Title)
	d.text(st.none.Bold(true), fmt.Sprintf("Review PR #%d (%s) in %s?", p.number, title, filepath.Base(p.repo)))
	d.blank()
	d.row("repo", paint(st.muted, p.repo))
	d.row("review", fmt.Sprintf("delamain review --pr %d, in the background; its log goes to the state dir", p.number))
	d.blank()
	d.buttons([2]string{"review", "enter"}, [2]string{"cancel", "esc"})
}

// prLines are the step of the dialog of r that lists the open PRs.
func (m watchModel) prLines(d *dialogWriter) {
	st, p := m.st, m.dialog.issue
	d.text(st.none.Bold(true), fmt.Sprintf("Which PR in %s?", filepath.Base(p.repo)))
	d.blank()
	d.row("repo", paint(st.muted, p.repo))
	d.blank()
	switch {
	case p.loading:
		d.text(st.muted, "loading PRs…")
	case len(p.prs) == 0 && p.err == "":
		d.text(st.muted, "no open PRs in "+filepath.Base(p.repo))
	}
	for i, pr := range p.prs {
		mark := "  "
		if i == p.listCursor {
			mark = paint(st.info.Bold(true), "› ")
		}
		head := fmt.Sprintf("#%d ", pr.Number)
		tail := " @" + clean(pr.Author)
		if pr.Draft {
			tail += " draft"
		}
		if m.activeRun(p.runRepo(), pr.Number) {
			tail += " active run"
		}
		room := max(d.inner-lipgloss.Width(mark+head+tail), 1)
		d.styled(mark + paint(st.info, head) + ansi.Truncate(clean(pr.Title), room, "…") + paint(st.muted, tail))
	}
	m.pickerError(d)
	d.buttons([2]string{"review", "enter"}, [2]string{"back", "esc"})
}
