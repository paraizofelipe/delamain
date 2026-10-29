package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
func startPicker(t *testing.T, acts *fakeWatchActions, repos []string, now *time.Time) tea.Model {
	t.Helper()
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return *now }, actions: acts, after: noTimers, repos: repos}
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	return settle(t, m, m.Init())
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
