package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/forge"
)

// pickIssueRepo opens the dialog of i on app, the only repository of daemon.repos, and picks it.
func pickIssueRepo(t *testing.T, acts *fakeWatchActions, now *time.Time, prForge func(context.Context, string) (forge.Forge, error)) (tea.Model, string) {
	t.Helper()
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	m := send(t, send(t, startReviewTUI(t, acts, nil, []string{app}, now, prForge), keyPress("i")), keyPress("enter"))
	return m, app
}

// stillWatchingIssues fails unless m still lists its run with the dialog of i open.
func stillWatchingIssues(t *testing.T, m tea.Model) {
	t.Helper()
	if view := screen(m); !strings.Contains(view, "#55") || !strings.Contains(view, "delamain run") {
		t.Fatalf("the watch goes on, with the dialog open:\n%s", view)
	}
}

func TestWatchStartIssueShouldListOpenIssuesOfChosenRepo(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	f := twoIssues()
	var asked []string
	acts := &fakeWatchActions{}
	m := send(t, startReviewTUI(t, acts, nil, []string{app, web}, &now, prForgeOf(f, &asked)), keyPress("i"))
	m = send(t, send(t, m, keyPress("j")), keyPress("enter"))
	text := pickerText(m)
	for _, want := range []string{"Which issue in web?", "repo " + web, "› #12 fix the parser", "#9 wip: watch", "[ next enter ] [ back esc ]"} {
		if !pickerHas(text, want) {
			t.Fatalf("the open issues of web are listed, %q:\n%s", want, screen(m))
		}
	}
	if strings.Index(text, "#12") > strings.Index(text, "#9") {
		t.Fatalf("the issues keep the order of the forge, newest first:\n%s", text)
	}
	if len(asked) != 1 || asked[0] != web || len(f.issueLimits) != 1 || f.issueLimits[0] != 50 || len(f.limits) != 0 {
		t.Fatalf("the forge of web lists up to 50 open issues, once, and no PR: %v %v %v", asked, f.issueLimits, f.limits)
	}

	m = send(t, send(t, m, keyPress("down")), keyPress("enter"))
	if text := pickerText(m); !pickerHas(text, "Start issue #9 in web?") {
		t.Fatalf("enter on an issue asks to start it:\n%s", screen(m))
	}
	afterGrace(&now)
	if send(t, m, keyPress("enter")); len(acts.started) != 1 || acts.started[0] != (startCall{web, 9}) {
		t.Fatalf("the issue picked is started in web: %v", acts.started)
	}
}

func TestWatchStartIssueShouldShowLoadingThenForgeErrorInDialog(t *testing.T) {
	now := watchNow
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	f := &fakePRForge{issues: []forge.Issue{{Number: 3, Title: "stale"}}, err: errors.New("github: GET /issues: 502 bad gateway\x1b]52;c;ZXZpbA==\x07")}
	var asked []string
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app}, &now, prForgeOf(f, &asked)), keyPress("i"))
	m, cmd := m.Update(keyPress("enter"))
	m, load := m.Update(cmd()) // the check of the repository, whose answer starts the load
	if text := pickerText(m); !pickerHas(text, "loading issues…") || load == nil {
		t.Fatalf("the issues are loading, off Update:\n%s", screen(m))
	}
	if len(f.issueLimits) != 0 {
		t.Fatal("the forge is asked in a tea.Cmd, off Update")
	}
	m = settle(t, m, load)
	text := pickerText(m)
	if !pickerHas(text, "github: GET /issues: 502 bad gateway]52;c;ZXZpbA==") || pickerHas(text, "loading issues…") || pickerHas(text, "no open issues") {
		t.Fatalf("the error of the forge is shown in the dialog, not an empty list:\n%s", screen(m))
	}
	if strings.ContainsAny(m.View(), "\x1b\x07") {
		t.Fatalf("the error reaches the screen without its control characters: %q", m.View())
	}
	stillWatchingIssues(t, m)

	token := func(context.Context, string) (forge.Forge, error) {
		return nil, errors.New("no token for github.com: set GH_TOKEN")
	}
	m, _ = pickIssueRepo(t, &fakeWatchActions{}, &now, token)
	if text := pickerText(m); !pickerHas(text, "no token for github.com: set GH_TOKEN") {
		t.Fatalf("a forge that cannot be opened says why:\n%s", screen(m))
	}
	stillWatchingIssues(t, m)
}

func TestWatchStartIssueShouldShowEmptyStateWhenNoIssuesOpen(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	var asked []string
	m, _ := pickIssueRepo(t, acts, &now, prForgeOf(&fakePRForge{}, &asked))
	text := pickerText(m)
	if !pickerHas(text, "Which issue in app?") || !pickerHas(text, "no open issues in app") || pickerHas(text, "loading issues…") {
		t.Fatalf("a repository without open issues says so:\n%s", screen(m))
	}
	afterGrace(&now)
	if m = send(t, send(t, m, keyPress("enter")), keyPress("enter")); len(acts.started) != 0 || !pickerHas(pickerText(m), "no open issues in app") {
		t.Fatalf("without an issue enter picks nothing: %v\n%s", acts.started, screen(m))
	}
	stillWatchingIssues(t, m)
}

func TestWatchStartIssueShouldStripControlSequencesFromIssueTitles(t *testing.T) {
	now := watchNow
	evil := "evil\x1b]52;c;ZXZpbA==\x07\x1b[2J\x1b[31mforged\u009b6n\x00"
	var asked []string
	m, _ := pickIssueRepo(t, &fakeWatchActions{}, &now, prForgeOf(&fakePRForge{issues: []forge.Issue{{Number: 7, Title: evil}}}, &asked))
	if view := m.View(); strings.ContainsAny(view, "\x1b\x07\u009b\x00") {
		t.Fatalf("no control sequence of the forge reaches the terminal: %q", view)
	}
	if text := pickerText(m); !pickerHas(text, "#7 evil]52;c;ZXZpbA==[2J[31mforged6n") {
		t.Fatalf("the title is shown without its control characters:\n%s", screen(m))
	}

	long := strings.Repeat("a long title ", 30)
	m, _ = pickIssueRepo(t, &fakeWatchActions{}, &now, prForgeOf(&fakePRForge{issues: []forge.Issue{{Number: 8, Title: long}}}, &asked))
	if text := pickerText(m); !pickerHas(text, "#8 a long title") || !strings.Contains(text, "…") || pickerHas(text, long) {
		t.Fatalf("a long title is cut to the width of the dialog:\n%s", screen(m))
	}
}

func TestWatchStartIssueShouldShowErrorWhenRepoHasNoForge(t *testing.T) {
	now := watchNow
	none := func(context.Context, string) (forge.Forge, error) { return nil, nil }
	m, app := pickIssueRepo(t, &fakeWatchActions{}, &now, none)
	if text := pickerText(m); !pickerHas(text, app+" has no GitHub or GitLab origin") || pickerHas(text, "no open issues") {
		t.Fatalf("a repository without origin has no issue to list:\n%s", screen(m))
	}
	stillWatchingIssues(t, m)
}

func TestWatchStartIssueShouldDropTheListOfAReplacedRepo(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	forges := map[string]*fakePRForge{
		app: {issues: []forge.Issue{{Number: 3, Title: "old app issue"}}},
		web: {issues: []forge.Issue{{Number: 8, Title: "web issue"}}},
	}
	byRepo := func(_ context.Context, repo string) (forge.Forge, error) { return forges[repo], nil }
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app, web}, &now, byRepo), keyPress("i"))
	m, check := m.Update(keyPress("enter"))
	m, load := m.Update(check())
	if load == nil {
		t.Fatalf("the issues of app load:\n%s", screen(m))
	}
	late := load() // the issues of app, answered after the user moved on

	m = send(t, send(t, send(t, m, keyPress("esc")), keyPress("j")), keyPress("enter"))
	m = send(t, m, late)
	if text := pickerText(m); !pickerHas(text, "Which issue in web?") || !pickerHas(text, "#8 web issue") || pickerHas(text, "#3") {
		t.Fatalf("the issues of app, loaded late, do not replace those of web:\n%s", screen(m))
	}

	m = send(t, m, keyPress("esc"))
	m, check = m.Update(keyPress("enter")) // app again, then the dialog closes before its issues come
	m, load = m.Update(check())
	m = send(t, send(t, m, keyPress("esc")), keyPress("esc"))
	if m = send(t, m, load()); strings.Contains(screen(m), "delamain run") {
		t.Fatalf("the issues of a closed dialog open nothing:\n%s", screen(m))
	}

}

func TestWatchStartIssueShouldBlockIssueWithActiveRun(t *testing.T) {
	now := watchNow
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	other := filepath.Join(filepath.Dir(app), "other")
	src := &fakeWatchSource{active: []RunView{
		owned(14, "#55", "a", 51230, true),
		runOn(20, "issue", app+"/", "#12"), // the same repository, written otherwise
		runOn(21, "review", app, "pr:9"),   // a review of PR 9, not issue 9
		runOn(22, "address", app, "addr:9"),
		runOn(23, "issue", other, "#7"), // another repository
	}}
	f := &fakePRForge{issues: []forge.Issue{
		{Number: 12, Title: "fix the parser"},
		{Number: 9, Title: "wip: watch"},
		{Number: 7, Title: "seven"},
	}}
	var asked []string
	acts := &fakeWatchActions{}
	m := send(t, send(t, startReviewTUI(t, acts, src, []string{app}, &now, prForgeOf(f, &asked)), keyPress("i")), keyPress("enter"))
	text := pickerText(m)
	if !pickerHas(text, "#12 fix the parser active run") {
		t.Fatalf("the list marks the issue with an active run:\n%s", screen(m))
	}
	if pickerHas(text, "#9 wip: watch active run") || pickerHas(text, "#7 seven active run") {
		t.Fatalf("a run of a PR, or an issue run of another repository, marks no issue:\n%s", screen(m))
	}

	afterGrace(&now)
	m = send(t, m, keyPress("enter"))
	if got := footerText(m)[0]; got != "a run is already active for issue 12" {
		t.Fatalf("enter on issue 12 says a run is active: %q", got)
	}
	if text := pickerText(m); !pickerHas(text, "Which issue in app?") || pickerHas(text, "Start issue #") {
		t.Fatalf("the list stays, with no confirmation:\n%s", screen(m))
	}

	// A run started after the list loaded is seen at the next reload.
	src.active = append(src.active, runOn(24, "issue", app, "#9"))
	m = send(t, reload(t, m), keyPress("down"))
	if m = send(t, m, keyPress("enter")); footerText(m)[0] != "a run is already active for issue 9" {
		t.Fatalf("enter checks the runs of the last reload:\n%s", screen(m))
	}
	m = send(t, send(t, m, keyPress("down")), keyPress("enter"))
	if !pickerHas(pickerText(m), "Start issue #7 in app?") {
		t.Fatalf("an issue without an active run of this repository can be started:\n%s", screen(m))
	}
	afterGrace(&now)
	if send(t, m, keyPress("enter")); len(acts.started) != 1 || acts.started[0] != (startCall{app, 7}) {
		t.Fatalf("only issue 7 is started: %v", acts.started)
	}
}
