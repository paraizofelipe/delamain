package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/forge"
)

// gitInit makes path a git repository with one commit.
func gitInit(t *testing.T, path string) string {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", path},
		{"-C", path, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return path
}

// startPicker is the TUI with repos as daemon.repos, after its first window size and reload.
// Every repository has the open issues of twoIssues.
func startPicker(t *testing.T, acts *fakeWatchActions, repos []string, now *time.Time) tea.Model {
	t.Helper()
	var asked []string
	return startReviewTUI(t, acts, nil, repos, now, prForgeOf(twoIssues(), &asked))
}

// twoIssues is a forge with two open issues, newest first.
func twoIssues() *fakePRForge {
	return &fakePRForge{issues: []forge.Issue{
		{Number: 12, Title: "fix the parser"},
		{Number: 9, Title: "wip: watch"},
	}}
}

// pickerText is the text of the dialog of i, without spaces: a long path wraps anywhere.
func pickerText(m tea.Model) string { return nospace(panelText(screen(m), "delamain run")) }

// pickerHas reports whether the text of pickerText shows want.
func pickerHas(text, want string) bool { return strings.Contains(text, nospace(want)) }

// typePath picks other path… when the list shows it, and submits path.
func typePath(t *testing.T, m tea.Model, path string) tea.Model {
	t.Helper()
	if pickerHas(pickerText(m), "other path…") {
		m = send(t, send(t, m, keyPress("G")), keyPress("enter"))
	}
	return send(t, send(t, m, keyPress(path)), keyPress("enter"))
}

func TestWatchPickerShouldListDaemonReposAndOtherPath(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	m := send(t, startPicker(t, &fakeWatchActions{}, []string{app, web}, &now), keyPress("i"))
	text := pickerText(m)
	for _, want := range []string{"› app " + app, "web " + web, "other path…", "[ pick enter ] [ cancel esc ]"} {
		if !pickerHas(text, want) {
			t.Fatalf("the picker lists %q:\n%s", want, screen(m))
		}
	}
	if strings.Index(text, web) > strings.Index(text, nospace("other path…")) {
		t.Fatalf("other path… comes last:\n%s", text)
	}

	m = send(t, send(t, m, keyPress("j")), keyPress("enter"))
	text = pickerText(m)
	for _, want := range []string{"Which issue in web?", "repo " + web, "issue", "[ next enter ] [ back esc ]"} {
		if !pickerHas(text, want) {
			t.Fatalf("a repo of the list asks for the issue, %q:\n%s", want, screen(m))
		}
	}

	m = send(t, m, keyPress("esc"))
	if text := pickerText(m); !pickerHas(text, "› web "+web) {
		t.Fatalf("esc on the issue goes back to the list, where it was:\n%s", screen(m))
	}
	m = send(t, send(t, m, keyPress("k")), keyPress("esc"))
	if strings.Contains(screen(m), "delamain run") {
		t.Fatalf("esc on the list closes the dialog:\n%s", screen(m))
	}
}

func TestWatchPickerShouldOpenPathInputWhenNoDaemonRepos(t *testing.T) {
	now := watchNow
	m := send(t, startPicker(t, &fakeWatchActions{}, nil, &now), keyPress("i"))
	text := pickerText(m)
	if pickerHas(text, "other path…") || !pickerHas(text, "Path of the repository") {
		t.Fatalf("without daemon.repos, i asks for a path at once:\n%s", screen(m))
	}
	// The keys of the screen and of the dialogs are letters of the path here.
	for _, k := range []string{"/", "j", "k", "q", "?", "s", "c"} {
		next, cmd := m.Update(keyPress(k))
		if quits(cmd) {
			t.Fatalf("%s is typed, it does not quit", k)
		}
		m = settle(t, next, cmd)
	}
	if text := pickerText(m); !pickerHas(text, "path /jkq?sc") {
		t.Fatalf("every key is typed in the path:\n%s", screen(m))
	}
	m = send(t, m, keyPress("esc"))
	if strings.Contains(screen(m), "delamain run") {
		t.Fatalf("esc on the path closes the dialog when there is no list to go back to:\n%s", screen(m))
	}
	m = send(t, send(t, m, keyPress("i")), tea.KeyMsg{Type: tea.KeyCtrlC})
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); !quits(cmd) {
		t.Fatal("ctrl+c still quits with the path open")
	}
}

func TestWatchPickerShouldRejectPathThatDoesNotExist(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app := gitInit(t, filepath.Join(dir, "app"))
	gone, file := filepath.Join(dir, "gone"), filepath.Join(app, "README")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := send(t, startPicker(t, &fakeWatchActions{}, []string{gone, app}, &now), keyPress("i"))

	m = send(t, m, keyPress("enter"))
	if text := pickerText(m); !pickerHas(text, gone+" does not exist") || !pickerHas(text, "› gone") {
		t.Fatalf("a repo of the list that is gone stays on the list, with why:\n%s", screen(m))
	}
	for path, why := range map[string]string{
		filepath.Join(dir, "missing"): " does not exist",
		file:                          " is not a directory",
		"":                            "type the path of a repository",
	} {
		m := typePath(t, send(t, startPicker(t, &fakeWatchActions{}, nil, &now), keyPress("i")), path)
		if text := pickerText(m); !pickerHas(text, path+why) || !pickerHas(text, "Path of the repository") {
			t.Fatalf("%q is refused with why, %q:\n%s", path, why, screen(m))
		}
	}
}

func TestWatchPickerShouldRejectPathOutsideGitRepository(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", dir) // the temp dir could be inside a repository
	plain := filepath.Join(dir, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	m := typePath(t, send(t, startPicker(t, &fakeWatchActions{}, nil, &now), keyPress("i")), plain)
	if text := pickerText(m); !pickerHas(text, plain+" is not inside a git repository") || pickerHas(text, "Which issue") {
		t.Fatalf("a directory outside git is refused:\n%s", screen(m))
	}

	sub := filepath.Join(gitInit(t, filepath.Join(dir, "app")), "cmd")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	m = send(t, send(t, startPicker(t, &fakeWatchActions{}, []string{plain}, &now), keyPress("i")), keyPress("enter"))
	if !pickerHas(pickerText(m), plain+" is not inside a git repository") {
		t.Fatalf("a repo of the list outside git is refused:\n%s", screen(m))
	}
	m = typePath(t, m, sub)
	if text := pickerText(m); !pickerHas(text, "Which issue in cmd?") || !pickerHas(text, "repo "+sub) {
		t.Fatalf("a directory inside a repository is a repo:\n%s", screen(m))
	}
}

func TestWatchPickerShouldExpandHomeInPath(t *testing.T) {
	now := watchNow
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := gitInit(t, filepath.Join(home, "app"))
	for _, typed := range []string{"~/app", "~/app/"} {
		m := typePath(t, send(t, startPicker(t, &fakeWatchActions{}, nil, &now), keyPress("i")), typed)
		if text := pickerText(m); !pickerHas(text, "Which issue in app?") || !pickerHas(text, "repo "+app) {
			t.Fatalf("%s is %s:\n%s", typed, app, screen(m))
		}
	}
	m := typePath(t, send(t, startPicker(t, &fakeWatchActions{}, nil, &now), keyPress("i")), "~")
	if text := pickerText(m); !pickerHas(text, home+" is not inside a git repository") {
		t.Fatalf("~ alone is the home: %s", text)
	}
}

// issueList opens the dialog of i on app, whose open issues are issues, and picks it.
func issueList(t *testing.T, acts *fakeWatchActions, now *time.Time, issues ...forge.Issue) (tea.Model, string) {
	t.Helper()
	var asked []string
	return pickIssueRepo(t, acts, now, prForgeOf(&fakePRForge{issues: issues}, &asked))
}

// fourIssues are open issues whose numbers share their first digits.
var fourIssues = []forge.Issue{
	{Number: 120, Title: "parse dates"},
	{Number: 12, Title: "fix the parser"},
	{Number: 9, Title: "wip: watch"},
	{Number: 31, Title: "Watch the queue"},
}

// typeKeys sends every key of keys, one by one.
func typeKeys(t *testing.T, m tea.Model, keys ...string) tea.Model {
	t.Helper()
	for _, k := range keys {
		m = send(t, m, keyPress(k))
	}
	return m
}

func TestWatchPickerListShouldFilterByNumberPrefix(t *testing.T) {
	now := watchNow
	m, _ := issueList(t, &fakeWatchActions{}, &now, fourIssues...)
	m = typeKeys(t, m, "down", "1")
	text := pickerText(m)
	for _, want := range []string{"filter 1█", "› #120 parse dates", "#12 fix the parser"} {
		if !pickerHas(text, want) {
			t.Fatalf("1 keeps the issues whose number starts with it, the cursor on the first, %q:\n%s", want, screen(m))
		}
	}
	if pickerHas(text, "#9 ") || pickerHas(text, "#31") {
		t.Fatalf("1 hides the issues whose number does not start with it:\n%s", screen(m))
	}

	m = typeKeys(t, m, "backspace", "#", "1", "2", "down")
	if text := pickerText(m); !pickerHas(text, "filter #12█") || !pickerHas(text, "#120 parse dates") || !pickerHas(text, "› #12 fix the parser") || pickerHas(text, "#31") {
		t.Fatalf("#12 keeps #120 and #12, and down moves on the issues left:\n%s", screen(m))
	}
	if m = send(t, m, keyPress("enter")); !pickerHas(pickerText(m), "Start issue #12 in app?") {
		t.Fatalf("enter picks the issue under the cursor of the list filtered:\n%s", screen(m))
	}
}

func TestWatchPickerListShouldFilterByTitleIgnoringCase(t *testing.T) {
	now := watchNow
	m, _ := issueList(t, &fakeWatchActions{}, &now, fourIssues...)
	m = typeKeys(t, m, "W", "a", "T", "C", "h")
	text := pickerText(m)
	if !pickerHas(text, "› #9 wip: watch") || !pickerHas(text, "#31 Watch the queue") || pickerHas(text, "#120") || pickerHas(text, "#12 ") {
		t.Fatalf("WaTCh keeps the issues whose title holds it, whatever the case:\n%s", screen(m))
	}

	var asked []string
	m, _ = pickApp(t, &fakeWatchActions{}, &now, prForgeOf(twoPRs(), &asked))
	m = typeKeys(t, m, "P", "A", "R", "S", "E", "R")
	if text := reviewText(m); !pickerHas(text, "› #12 fix the parser @ana") || pickerHas(text, "#9") {
		t.Fatalf("the list of r is filtered by the title too:\n%s", screen(m))
	}
}

func TestWatchPickerListShouldShowNoMatchWhenFilterEmptiesList(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	m, _ := issueList(t, acts, &now, fourIssues...)
	// j, k, g and G are letters of the filter here: ↑, ↓, home and end move the cursor.
	m = typeKeys(t, m, "j", "k", "g", "G", "q", "?")
	text := pickerText(m)
	if !pickerHas(text, "filter jkgGq?█") || !pickerHas(text, "no match") || pickerHas(text, "no open issues") || pickerHas(text, "#12") {
		t.Fatalf("a filter that matches no issue says so:\n%s", screen(m))
	}
	afterGrace(&now)
	if m = send(t, m, keyPress("enter")); len(acts.started) != 0 || pickerHas(pickerText(m), "Start issue") {
		t.Fatalf("enter without a line picks nothing: %v\n%s", acts.started, screen(m))
	}
	stillWatchingIssues(t, m)
}

func TestWatchPickerListShouldNavigateWithArrowsHomeAndEnd(t *testing.T) {
	now := watchNow
	m, _ := issueList(t, &fakeWatchActions{}, &now, fourIssues...)
	for _, c := range []struct{ key, want string }{{"end", "› #31"}, {"up", "› #9"}, {"home", "› #120"}, {"down", "› #12 fix"}} {
		if m = send(t, m, keyPress(c.key)); !pickerHas(pickerText(m), c.want) {
			t.Fatalf("%s puts the cursor on %q:\n%s", c.key, c.want, screen(m))
		}
	}
}

func TestWatchPickerListEscShouldClearFilterThenGoBack(t *testing.T) {
	now := watchNow
	m, app := issueList(t, &fakeWatchActions{}, &now, fourIssues...)
	m = typeKeys(t, m, "w", "i", "p", "esc")
	text := pickerText(m)
	if !pickerHas(text, "Which issue in app?") || !pickerHas(text, "filter █") || !pickerHas(text, "› #120 parse dates") || !pickerHas(text, "#31 Watch the queue") {
		t.Fatalf("esc with a filter clears it and stays on the list:\n%s", screen(m))
	}
	m = send(t, m, keyPress("esc"))
	if text := pickerText(m); !pickerHas(text, "Start an issue run in which repository?") || !pickerHas(text, "› app "+app) {
		t.Fatalf("esc without a filter goes back to the repository:\n%s", screen(m))
	}
	if m = send(t, m, keyPress("enter")); !pickerHas(pickerText(m), "filter █") {
		t.Fatalf("the list of the repository picked again starts with no filter:\n%s", screen(m))
	}

	var asked []string
	m = typePath(t, send(t, startReviewTUI(t, &fakeWatchActions{}, nil, nil, &now, prForgeOf(twoPRs(), &asked)), keyPress("r")), app)
	if text := reviewText(m); !pickerHas(text, "filter █") || pickerHas(text, "filter "+app) {
		t.Fatalf("the path typed is no filter of the list:\n%s", screen(m))
	}
	m = typeKeys(t, m, "a", "n", "a", "esc", "esc")
	if text := reviewText(m); !pickerHas(text, "Path of the repository") || !pickerHas(text, "path "+app) {
		t.Fatalf("on r too, esc clears the filter, then goes back to the path typed:\n%s", screen(m))
	}
}
