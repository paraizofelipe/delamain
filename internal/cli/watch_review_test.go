package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/forge"
)

// fakePRForge is the forge of the repository picked in the dialog of r: it only lists PRs.
type fakePRForge struct {
	forge.Forge
	mu     sync.Mutex
	prs    []forge.PRDetail
	err    error
	limits []int // the limit of every ListOpenPRs
}

func (f *fakePRForge) ListOpenPRs(_ context.Context, limit int) ([]forge.PRDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, limit)
	return f.prs, f.err
}

// prForgeOf is the prForge of watch answering f for every repository, recording which.
func prForgeOf(f *fakePRForge, asked *[]string) func(context.Context, string) (forge.Forge, error) {
	return func(_ context.Context, repo string) (forge.Forge, error) {
		*asked = append(*asked, repo)
		return f, nil
	}
}

// startReviewTUI is the TUI with repos as daemon.repos and prForge as the forge of every
// repository, after its first window size and reload.
func startReviewTUI(t *testing.T, acts *fakeWatchActions, src *fakeWatchSource, repos []string, now *time.Time, prForge func(context.Context, string) (forge.Forge, error)) tea.Model {
	t.Helper()
	if src == nil {
		src = &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	}
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return *now }, actions: acts, after: noTimers, repos: repos, prForge: prForge}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	return settle(t, m, m.Init())
}

// reviewText is the text of the dialog of r, without spaces: a long path wraps anywhere.
func reviewText(m tea.Model) string { return nospace(panelText(screen(m), "delamain review")) }

func TestWatchStartReviewShouldListOpenPRsOfChosenRepo(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	f := &fakePRForge{prs: []forge.PRDetail{
		{Number: 12, Title: "fix the parser", Author: "ana"},
		{Number: 9, Title: "wip: watch", Author: "bo", Draft: true},
	}}
	var asked []string
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app, web}, &now, prForgeOf(f, &asked)), keyPress("r"))
	text := reviewText(m)
	for _, want := range []string{"Review a PR in which repository?", "› app " + app, "web " + web, "other path…", "[ pick enter ] [ cancel esc ]"} {
		if !pickerHas(text, want) {
			t.Fatalf("r opens the picker of the repository, %q:\n%s", want, screen(m))
		}
	}

	m = send(t, send(t, m, keyPress("j")), keyPress("enter"))
	text = reviewText(m)
	for _, want := range []string{"Which PR in web?", "repo " + web, "› #12 fix the parser @ana", "#9 wip: watch @bo draft"} {
		if !pickerHas(text, want) {
			t.Fatalf("the open PRs of web are listed, %q:\n%s", want, screen(m))
		}
	}
	if strings.Index(text, "#12") > strings.Index(text, "#9") {
		t.Fatalf("the PRs keep the order of the forge, newest first:\n%s", text)
	}
	if len(asked) != 1 || asked[0] != web || len(f.limits) != 1 || f.limits[0] != 50 {
		t.Fatalf("the forge of web lists up to 50 open PRs, once: %v %v", asked, f.limits)
	}
}

func TestWatchStartReviewShouldShowLoadingWhileFetchingPRs(t *testing.T) {
	now := watchNow
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	f := &fakePRForge{prs: []forge.PRDetail{{Number: 12, Title: "fix the parser", Author: "ana"}}}
	var asked []string
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app}, &now, prForgeOf(f, &asked)), keyPress("r"))
	m, cmd := m.Update(keyPress("enter"))
	if cmd == nil {
		t.Fatalf("enter on app checks the repository:\n%s", screen(m))
	}
	checked := cmd() // the check of the repository, whose answer starts the load
	m, load := m.Update(checked)
	if text := reviewText(m); !pickerHas(text, "loading PRs…") || pickerHas(text, "#12") || load == nil {
		t.Fatalf("the PRs are loading, off Update:\n%s", screen(m))
	}
	if len(f.limits) != 0 {
		t.Fatal("the forge is asked in a tea.Cmd, off Update")
	}
	m = settle(t, m, load)
	if text := reviewText(m); pickerHas(text, "loading PRs…") || !pickerHas(text, "#12 fix the parser @ana") {
		t.Fatalf("the list replaces the loading:\n%s", screen(m))
	}
}

func TestWatchStartReviewShouldStripControlSequencesFromPRTitles(t *testing.T) {
	now := watchNow
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	evil := "evil\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mforged\u009b6n\x00"
	f := &fakePRForge{prs: []forge.PRDetail{{Number: 7, Title: evil, Author: "mallory\x1b[5m\x07"}}}
	var asked []string
	m := send(t, send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app}, &now, prForgeOf(f, &asked)), keyPress("r")), keyPress("enter"))
	if view := m.View(); strings.ContainsAny(view, "\x1b\x07\u009b\x00") {
		t.Fatalf("no control sequence of the forge reaches the terminal: %q", view)
	}
	if text := reviewText(m); !pickerHas(text, "#7 evil]52;c;ZXZpbA==[2J[31mforged6n @mallory[5m") {
		t.Fatalf("the title and the author are shown without their control characters:\n%s", screen(m))
	}
}

// pickApp opens the dialog of r on app, the only repository of daemon.repos, and picks it.
func pickApp(t *testing.T, acts *fakeWatchActions, src *fakeWatchSource, now *time.Time, prForge func(context.Context, string) (forge.Forge, error)) (tea.Model, string) {
	t.Helper()
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	m := send(t, send(t, startReviewTUI(t, acts, src, []string{app}, now, prForge), keyPress("r")), keyPress("enter"))
	return m, app
}

// stillWatching fails unless m still lists its run and answers the keys of the dialog of r.
func stillWatching(t *testing.T, m tea.Model) {
	t.Helper()
	if view := screen(m); !strings.Contains(view, "#55") || !strings.Contains(view, "delamain review") {
		t.Fatalf("the watch goes on, with the dialog open:\n%s", view)
	}
}

func TestWatchStartReviewShouldShowEmptyStateWhenNoPRsOpen(t *testing.T) {
	now := watchNow
	var asked []string
	m, _ := pickApp(t, &fakeWatchActions{}, nil, &now, prForgeOf(&fakePRForge{}, &asked))
	text := reviewText(m)
	for _, want := range []string{"Which PR in app?", "no open PRs in app", "[ review enter ] [ back esc ]"} {
		if !pickerHas(text, want) {
			t.Fatalf("a repository without open PRs says so, %q:\n%s", want, screen(m))
		}
	}
	if pickerHas(text, "loading PRs…") {
		t.Fatalf("the load ended:\n%s", screen(m))
	}
	stillWatching(t, m)
}

func TestWatchStartReviewShouldShowForgeErrorInDialog(t *testing.T) {
	now := watchNow
	var asked []string
	f := &fakePRForge{err: errors.New("github: GET /pulls: 502 bad gateway\x1b]52;c;ZXZpbA==\x07")}
	m, _ := pickApp(t, &fakeWatchActions{}, nil, &now, prForgeOf(f, &asked))
	text := reviewText(m)
	if !pickerHas(text, "github: GET /pulls: 502 bad gateway]52;c;ZXZpbA==") || pickerHas(text, "no open PRs") || pickerHas(text, "loading PRs…") {
		t.Fatalf("the error of the forge is shown in the dialog, not an empty list:\n%s", screen(m))
	}
	if strings.ContainsAny(m.View(), "\x1b\x07") {
		t.Fatalf("the error reaches the screen without its control characters: %q", m.View())
	}
	stillWatching(t, m)

	token := func(context.Context, string) (forge.Forge, error) {
		return nil, errors.New("no token for github.com: set GH_TOKEN")
	}
	m, _ = pickApp(t, &fakeWatchActions{}, nil, &now, token)
	if text := reviewText(m); !pickerHas(text, "no token for github.com: set GH_TOKEN") {
		t.Fatalf("a forge that cannot be opened says why:\n%s", screen(m))
	}
	stillWatching(t, m)
}

func TestWatchStartReviewShouldShowErrorWhenRepoHasNoForge(t *testing.T) {
	now := watchNow
	none := func(context.Context, string) (forge.Forge, error) { return nil, nil }
	m, app := pickApp(t, &fakeWatchActions{}, nil, &now, none)
	if text := reviewText(m); !pickerHas(text, app+" has no GitHub or GitLab origin") || pickerHas(text, "no open PRs") {
		t.Fatalf("a repository without origin has no PR to review:\n%s", screen(m))
	}
	stillWatching(t, m)
}
