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
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/gitx"
)

// pickStep is where the dialog of i stands: the repository, then the issue.
type pickStep int

const (
	pickRepo    pickStep = iota // the list of daemon.repos and other path…
	pickPath                    // the path typed by hand
	pickIssue                   // the number of the issue
	pickConfirm                 // start the run?
)

// issuePicker is the state of the dialog of i: which repository, then which issue.
type issuePicker struct {
	step     pickStep
	cursor   int       // the line of the list: a repo of daemon.repos, or other path…
	input    lineInput // the path, then the issue
	path     string    // the path typed, kept to go back to it
	repo     string    // the absolute path of the repository picked
	number   int       // the issue to start
	err      string    // why the path or the issue was refused
	checking bool      // the path is being checked: enter waits for it
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
	repo string
	err  error
}

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

// openStartIssue opens the dialog of i on the list of daemon.repos, or on the path without one.
func (m watchModel) openStartIssue() watchModel {
	p := issuePicker{}
	if len(m.opts.repos) == 0 {
		p.step, p.input = pickPath, newPickerInput("")
	}
	m.dialog = watchDialog{kind: dialogStartIssue, at: m.opts.now(), issue: p}
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
			return m, m.checkRepo(m.opts.repos[p.cursor])
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
			return m, m.checkRepo(p.path)
		}
		p.input = p.input.Update(msg)
		return m, nil
	case pickConfirm:
		return m.confirmKey(msg)
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

// confirmKey handles a key of the confirmation: enter or y starts the run, off Update, and closes
// the dialog. The run is no stop, remove or recover of the store: it starts beside them.
func (m watchModel) confirmKey(msg tea.KeyMsg) (watchModel, tea.Cmd) {
	switch {
	case key.Matches(msg, pickerKeys.cancel):
		m.dialog = watchDialog{}
		return m, nil
	case !key.Matches(msg, pickerKeys.start), m.opts.now().Sub(m.dialog.at) < confirmGrace:
		return m, nil
	}
	repo, issue, acts := m.dialog.issue.repo, m.dialog.issue.number, m.opts.actions
	m.dialog = watchDialog{}
	m, toast := m.showToast(m.st.warn, fmt.Sprintf("starting issue #%d in %s…", issue, filepath.Base(repo)))
	return m, tea.Batch(toast, func() tea.Msg {
		if err := acts.StartIssue(repo, issue); err != nil {
			return watchQuickMsg{err: err} // the code and the log of a run that failed
		}
		return nil
	})
}

// checkRepo checks path off Update: it is answered by a watchRepoMsg.
func (m *watchModel) checkRepo(path string) tea.Cmd {
	m.dialog.issue.checking = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), repoCheckFor)
		defer cancel()
		repo, err := checkRepoDir(ctx, path)
		return watchRepoMsg{repo: repo, err: err}
	}
}

// repoChecked moves on to the issue with the repository checked, or stays with why it was refused.
func (m watchModel) repoChecked(msg watchRepoMsg) watchModel {
	if m.dialog.kind != dialogStartIssue || !m.dialog.issue.checking {
		return m // the dialog closed while the path was checked
	}
	p := &m.dialog.issue
	p.checking = false
	if msg.err != nil {
		p.err = clean(msg.err.Error())
		return m
	}
	p.step, p.repo, p.err, p.input = pickIssue, msg.repo, "", newPickerInput("")
	return m
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
		d.text(st.none.Bold(true), "Start an issue run in which repository?")
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
		d.text(st.none.Bold(true), fmt.Sprintf("Start issue #%d in %s?", p.number, filepath.Base(p.repo)))
		d.blank()
		d.row("repo", paint(st.muted, p.repo))
		d.row("run", fmt.Sprintf("delamain run --issue %d, in the background; its log goes to the state dir", p.number))
		d.blank()
		d.buttons([2]string{"start", "enter"}, [2]string{"cancel", "esc"})
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
