package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// pickStep is where the dialog of i or r stands: the repository, then the issue or the PR.
type pickStep int

const (
	pickRepo    pickStep = iota // the list of daemon.repos and other path…
	pickPath                    // the path typed by hand
	pickIssue                   // the number of the issue
	pickConfirm                 // start the run, or the review?
	pickPR                      // the open PRs of the repository
)

// issuePicker is the state of the dialog of i or r: which repository, then which issue or PR.
type issuePicker struct {
	step     pickStep
	cursor   int              // the line of the list: a repo of daemon.repos, or other path…
	input    lineInput        // the path, then the issue
	path     string           // the path typed, kept to go back to it
	repo     string           // the absolute path of the repository picked
	number   int              // the issue to start, or the PR to review
	err      string           // why the path or the issue was refused
	checking bool             // the path is being checked: enter waits for it
	checkID  uint64           // the check awaited: the answer of an earlier one is dropped
	prs      []forge.PRDetail // the open PRs of repo, for r
	prCursor int              // the line of the list of PRs
	loading  bool             // the PRs are being listed
	loadID   uint64           // the load awaited: the answer of an earlier one is dropped
	top      string           // the main worktree of repo, where the runs of its PRs are recorded
}

// runRepo is the repository the runs of the PRs of repo are recorded under: the main worktree,
// or repo itself while it is not resolved.
func (p issuePicker) runRepo() string {
	if p.top != "" {
		return p.top
	}
	return p.repo
}

// pickerKeys are the keys of the dialog of i.
var pickerKeys = struct {
	next, back, start, cancel key.Binding
}{
	next:   key.NewBinding(key.WithKeys("enter")),
	back:   key.NewBinding(key.WithKeys("esc")),
	start:  key.NewBinding(key.WithKeys("enter", "y")),
	cancel: key.NewBinding(key.WithKeys("esc", "n")),
}

// repoCheckFor is how long the check of a path may take: git on a dead network mount would hang.
const repoCheckFor = 5 * time.Second

// watchRepoMsg is how the check of a path ended: the repository, or why it is refused.
type watchRepoMsg struct {
	id   uint64 // the check it answers
	repo string
	err  error
}

// repoCheckSeq numbers the checks and the loads of PRs, across dialogs: a reopened one starts its
// picker from zero.
var repoCheckSeq atomic.Uint64

// lineInput is a line typed at its end: the path or the issue. It needs no cursor that moves,
// nor one that blinks and asks for a timer on every key.
type lineInput struct{ value []rune }

func newPickerInput(value string) lineInput { return lineInput{value: []rune(value)} }

// Value is the line typed.
func (in lineInput) Value() string { return string(in.value) }

// Update types the letters of msg, pasted ones included, and erases one with backspace. Only
// printable letters are kept: a pasted line break or escape is no part of a path.
func (in lineInput) Update(msg tea.KeyMsg) lineInput {
	switch msg.Type {
	case tea.KeyBackspace:
		if len(in.value) > 0 {
			in.value = in.value[:len(in.value)-1]
		}
	case tea.KeySpace:
		in.value = append(in.value, ' ')
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if unicode.IsPrint(r) {
				in.value = append(in.value, r)
			}
		}
	}
	return in
}

// View is the line and the cursor at its end.
func (in lineInput) View() string { return string(in.value) + "█" }

// openPicker opens the dialog of i or r, by kind, on the list of daemon.repos, or on the path
// without one.
func (m watchModel) openPicker(kind dialogKind) watchModel {
	p := issuePicker{}
	if len(m.opts.repos) == 0 {
		p.step, p.input = pickPath, newPickerInput("")
	}
	m.dialog = watchDialog{kind: kind, at: m.opts.now(), issue: p}
	return m
}

// issueKey handles a key of the dialog of i. Every key goes to it: in a text input, j, k, q and ?
// are letters.
func (m watchModel) issueKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	p := &m.dialog.issue
	switch p.step {
	case pickRepo:
		switch {
		case key.Matches(msg, pickerKeys.back):
			m.dialog = watchDialog{}
		case key.Matches(msg, watchKeys.up):
			p.cursor = max(p.cursor-1, 0)
		case key.Matches(msg, watchKeys.down):
			p.cursor = min(p.cursor+1, len(m.opts.repos))
		case key.Matches(msg, watchKeys.first):
			p.cursor = 0
		case key.Matches(msg, watchKeys.last):
			p.cursor = len(m.opts.repos)
		case key.Matches(msg, pickerKeys.next) && !p.checking:
			if p.cursor == len(m.opts.repos) {
				p.step, p.err, p.input = pickPath, "", newPickerInput(p.path)
				return m, nil
			}
			cmd := m.checkRepo(m.opts.repos[p.cursor]) // before m is copied into the return: it sets checking
			return m, cmd
		}
		return m, nil
	case pickPath:
		switch {
		case key.Matches(msg, pickerKeys.back):
			if len(m.opts.repos) == 0 {
				m.dialog = watchDialog{}
			} else {
				p.step, p.err, p.path, p.checking = pickRepo, "", p.input.Value(), false
			}
			return m, nil
		case key.Matches(msg, pickerKeys.next):
			if p.checking {
				return m, nil
			}
			p.path = p.input.Value()
			cmd := m.checkRepo(p.path) // before m is copied into the return: it sets checking
			return m, cmd
		}
		p.input = p.input.Update(msg)
		return m, nil
	case pickConfirm:
		return m.confirmKey(msg)
	case pickPR:
		return m.prKey(msg)
	}
	switch {
	case key.Matches(msg, pickerKeys.next):
		n, err := strconv.Atoi(strings.TrimSpace(p.input.Value()))
		if err != nil || n <= 0 {
			p.err = "the issue is a number greater than 0"
			return m, nil
		}
		p.step, p.number, p.err = pickConfirm, n, ""
		m.dialog.at = m.opts.now() // the enter of the number, held, must not answer the confirmation
		return m, nil
	case key.Matches(msg, pickerKeys.back):
		p.step, p.err = pickRepo, ""
		if len(m.opts.repos) == 0 || p.cursor == len(m.opts.repos) {
			p.step, p.input = pickPath, newPickerInput(p.path)
		}
		return m, nil
	}
	p.input = p.input.Update(msg)
	return m, nil
}

// confirmKey handles a key of the confirmation: enter or y starts the run or the review, off
// Update, and closes the dialog. It is no stop, remove or recover of the store: it starts beside
// them.
func (m watchModel) confirmKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	switch {
	case key.Matches(msg, pickerKeys.cancel):
		m.dialog = watchDialog{}
		return m, nil
	case !key.Matches(msg, pickerKeys.start), m.opts.now().Sub(m.dialog.at) < confirmGrace:
		return m, nil
	}
	repo, n, acts := m.dialog.issue.repo, m.dialog.issue.number, m.opts.actions
	text, start := fmt.Sprintf("starting issue #%d in %s…", n, filepath.Base(repo)), acts.StartIssue
	if m.dialog.kind == dialogStartReview {
		text, start = fmt.Sprintf("starting review of PR #%d in %s…", n, filepath.Base(repo)), acts.StartReview
	}
	m.dialog = watchDialog{}
	m, toast := m.showToast(m.st.warn, text)
	return m, tea.Batch(toast, func() tea.Msg {
		if err := start(repo, n); err != nil {
			return watchQuickMsg{err: err} // the code and the log of a run that failed
		}
		return nil
	})
}

// checkRepo checks path off Update: it is answered by a watchRepoMsg.
func (m *watchModel) checkRepo(path string) tea.Cmd {
	id := repoCheckSeq.Add(1)
	m.dialog.issue.checking, m.dialog.issue.checkID = true, id
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), repoCheckFor)
		defer cancel()
		repo, err := checkRepoDir(ctx, path)
		return watchRepoMsg{id: id, repo: repo, err: err}
	}
}

// repoChecked moves on to the issue, or to the PRs it starts to load, with the repository checked,
// or stays with why it was refused.
func (m watchModel) repoChecked(msg watchRepoMsg) (watchModel, tea.Cmd) {
	kind := m.dialog.kind
	if kind != dialogStartIssue && kind != dialogStartReview || !m.dialog.issue.checking || msg.id != m.dialog.issue.checkID {
		return m, nil // the dialog closed, or the check was replaced, while the path was checked
	}
	p := &m.dialog.issue
	p.checking = false
	if msg.err != nil {
		p.err = clean(msg.err.Error())
		return m, nil
	}
	if kind == dialogStartReview {
		p.step, p.repo, p.err = pickPR, msg.repo, ""
		cmd := m.loadPRs() // before m is copied into the return: it sets loading
		return m, cmd
	}
	p.step, p.repo, p.err, p.input = pickIssue, msg.repo, "", newPickerInput("")
	return m, nil
}

// checkRepoDir is the absolute path of path, ~ expanded, when it is a directory inside a git
// repository. Absolute, it never starts with -: run would read it as a flag.
func checkRepoDir(ctx context.Context, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("type the path of a repository")
	}
	abs, err := filepath.Abs(homePath(path))
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s does not exist", abs)
	case err != nil:
		return "", err
	case !fi.IsDir():
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	if _, err := gitx.New().Toplevel(ctx, abs); err != nil {
		return "", fmt.Errorf("%s is not inside a git repository", abs)
	}
	return abs, nil
}

// homePath is path with a leading ~ as the home directory, as the config expands it.
func homePath(path string) string {
	home, err := os.UserHomeDir()
	switch {
	case err != nil:
		return path
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	}
	return path
}

// pickerLines are the dialog of i, at its step.
func (m watchModel) pickerLines(d *dialogWriter) {
	st, p := m.st, m.dialog.issue
	switch p.step {
	case pickRepo:
		question := "Start an issue run in which repository?"
		if m.dialog.kind == dialogStartReview {
			question = "Review a PR in which repository?"
		}
		d.text(st.none.Bold(true), question)
		d.blank()
		for i, r := range append(append([]string{}, m.opts.repos...), "") {
			mark, name := "  ", filepath.Base(r)
			if i == p.cursor {
				mark = paint(st.info.Bold(true), "› ")
			}
			if r == "" {
				d.styled(mark + "other path…")
				continue
			}
			d.styled(mark + paint(st.none.Bold(true), name) + " " + paint(st.muted, r))
		}
		m.pickerError(d)
		d.buttons([2]string{"pick", "enter"}, [2]string{"cancel", "esc"})
	case pickPath:
		d.text(st.none.Bold(true), "Path of the repository")
		d.blank()
		d.row("path", p.input.View())
		m.pickerError(d)
		back := "back"
		if len(m.opts.repos) == 0 {
			back = "cancel"
		}
		d.buttons([2]string{"next", "enter"}, [2]string{back, "esc"})
	case pickConfirm:
		if m.dialog.kind == dialogStartReview {
			m.reviewLines(d)
			return
		}
		d.text(st.none.Bold(true), fmt.Sprintf("Start issue #%d in %s?", p.number, filepath.Base(p.repo)))
		d.blank()
		d.row("repo", paint(st.muted, p.repo))
		d.row("run", fmt.Sprintf("delamain run --issue %d, in the background; its log goes to the state dir", p.number))
		d.blank()
		d.buttons([2]string{"start", "enter"}, [2]string{"cancel", "esc"})
	case pickPR:
		m.prLines(d)
	default:
		d.text(st.none.Bold(true), fmt.Sprintf("Which issue in %s?", filepath.Base(p.repo)))
		d.blank()
		d.row("repo", paint(st.muted, p.repo))
		d.row("issue", p.input.View())
		m.pickerError(d)
		d.buttons([2]string{"next", "enter"}, [2]string{"back", "esc"})
	}
}

// pickerError writes why the last answer was refused, and the blank line before the buttons.
func (m watchModel) pickerError(d *dialogWriter) {
	d.blank()
	if e := m.dialog.issue.err; e != "" {
		d.text(m.st.bad, e)
		d.blank()
	}
}
