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

// fakePRForge is the forge of the repository picked in the dialog of i or r: it only lists the
// open issues and PRs.
type fakePRForge struct {
	forge.Forge
	mu          sync.Mutex
	prs         []forge.PRDetail
	issues      []forge.Issue
	err         error
	limits      []int // the limit of every ListOpenPRs
	issueLimits []int // the limit of every ListOpenIssues
}

func (f *fakePRForge) ListOpenIssues(_ context.Context, limit int) ([]forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueLimits = append(f.issueLimits, limit)
	return f.issues, f.err
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
func pickApp(t *testing.T, acts *fakeWatchActions, now *time.Time, prForge func(context.Context, string) (forge.Forge, error)) (tea.Model, string) {
	t.Helper()
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	m := send(t, send(t, startReviewTUI(t, acts, nil, []string{app}, now, prForge), keyPress("r")), keyPress("enter"))
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
	m, _ := pickApp(t, &fakeWatchActions{}, &now, prForgeOf(&fakePRForge{}, &asked))
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
	m, _ := pickApp(t, &fakeWatchActions{}, &now, prForgeOf(f, &asked))
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
	m, _ = pickApp(t, &fakeWatchActions{}, &now, token)
	if text := reviewText(m); !pickerHas(text, "no token for github.com: set GH_TOKEN") {
		t.Fatalf("a forge that cannot be opened says why:\n%s", screen(m))
	}
	stillWatching(t, m)
}

func TestWatchStartReviewShouldShowErrorWhenRepoHasNoForge(t *testing.T) {
	now := watchNow
	none := func(context.Context, string) (forge.Forge, error) { return nil, nil }
	m, app := pickApp(t, &fakeWatchActions{}, &now, none)
	if text := reviewText(m); !pickerHas(text, app+" has no GitHub or GitLab origin") || pickerHas(text, "no open PRs") {
		t.Fatalf("a repository without origin has no PR to review:\n%s", screen(m))
	}
	stillWatching(t, m)
}

// twoPRs is a forge with two open PRs, newest first.
func twoPRs() *fakePRForge {
	return &fakePRForge{prs: []forge.PRDetail{
		{Number: 12, Title: "fix the parser", Author: "ana"},
		{Number: 9, Title: "wip: watch", Author: "bo", Draft: true},
	}}
}

func TestWatchStartReviewShouldCallStartReviewOnConfirm(t *testing.T) {
	for _, confirm := range []string{"enter", "y"} {
		now := watchNow
		acts := &fakeWatchActions{}
		var asked []string
		m, app := pickApp(t, acts, &now, prForgeOf(twoPRs(), &asked))
		m = send(t, send(t, send(t, m, keyPress("G")), keyPress("j")), keyPress("g"))
		if !pickerHas(reviewText(m), "› #12") {
			t.Fatalf("g goes back to the first PR:\n%s", screen(m))
		}
		m = send(t, send(t, m, keyPress("down")), keyPress("enter"))
		text := reviewText(m)
		for _, want := range []string{"Review PR #9 (wip: watch) in app?", "repo " + app, "[ review enter ] [ cancel esc ]"} {
			if !pickerHas(text, want) {
				t.Fatalf("the confirmation says %q:\n%s", want, screen(m))
			}
		}
		// The enter that picked the PR, held or tapped twice, must not answer the confirmation.
		if m = send(t, m, keyPress(confirm)); len(acts.reviewed) != 0 || !pickerHas(reviewText(m), "Review PR #9") {
			t.Fatalf("%s this soon is ignored: %v", confirm, acts.reviewed)
		}
		afterGrace(&now)
		next, cmd := m.Update(keyPress(confirm))
		if view := screen(next); strings.Contains(view, "delamain review") || footerText(next)[0] != "starting review of PR #9 in app…" {
			t.Fatalf("%s closes the dialog and the footer says the review is starting:\n%s", confirm, view)
		}
		if len(acts.reviewed) != 0 {
			t.Fatal("the review starts in a tea.Cmd, off Update")
		}
		settle(t, next, cmd)
		if len(acts.reviewed) != 1 || acts.reviewed[0] != (reviewCall{app, 9}) {
			t.Fatalf("%s starts the review of PR 9 in %s, once: %v", confirm, app, acts.reviewed)
		}
		if len(acts.started) != 0 || len(acts.stops) != 0 || len(acts.removes) != 0 || len(acts.recovers) != 0 {
			t.Fatalf("%s starts no issue and touches no run: %+v", confirm, acts)
		}
	}

	for _, cancel := range []string{"esc", "n"} {
		now := watchNow
		acts := &fakeWatchActions{}
		var asked []string
		m, _ := pickApp(t, acts, &now, prForgeOf(twoPRs(), &asked))
		afterGrace(&now)
		if m = send(t, send(t, m, keyPress("enter")), keyPress(cancel)); strings.Contains(screen(m), "delamain review") || len(acts.reviewed) != 0 {
			t.Fatalf("%s closes the confirmation and reviews nothing: %v\n%s", cancel, acts.reviewed, screen(m))
		}
	}

	now := watchNow
	acts := &fakeWatchActions{}
	var asked []string
	m, _ := pickApp(t, acts, &now, prForgeOf(&fakePRForge{}, &asked))
	afterGrace(&now)
	if m = send(t, send(t, m, keyPress("enter")), keyPress("enter")); !pickerHas(reviewText(m), "no open PRs in app") || len(acts.reviewed) != 0 {
		t.Fatalf("without a PR enter picks nothing: %v\n%s", acts.reviewed, screen(m))
	}
}

func TestWatchStartReviewShouldGoBackToPickerOnEsc(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	var asked []string
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app, web}, &now, prForgeOf(twoPRs(), &asked)), keyPress("r"))
	m = send(t, send(t, send(t, m, keyPress("j")), keyPress("enter")), keyPress("esc"))
	if text := reviewText(m); !pickerHas(text, "Review a PR in which repository?") || !pickerHas(text, "› web "+web) {
		t.Fatalf("esc on the PRs goes back to the list of repositories, where it was:\n%s", screen(m))
	}
	m = send(t, m, keyPress("esc"))
	if strings.Contains(screen(m), "delamain review") {
		t.Fatalf("esc on the list closes the dialog:\n%s", screen(m))
	}

	m = typePath(t, send(t, startReviewTUI(t, &fakeWatchActions{}, nil, nil, &now, prForgeOf(twoPRs(), &asked)), keyPress("r")), app)
	if !pickerHas(reviewText(m), "#12 fix the parser") {
		t.Fatalf("a path typed lists its PRs:\n%s", screen(m))
	}
	m = send(t, m, keyPress("esc"))
	if text := reviewText(m); !pickerHas(text, "Path of the repository") || !pickerHas(text, "path "+app) {
		t.Fatalf("without daemon.repos esc goes back to the path typed:\n%s", screen(m))
	}
}

func TestWatchStartReviewShouldDropTheListOfAReplacedRepo(t *testing.T) {
	now := watchNow
	dir := t.TempDir()
	app, web := gitInit(t, filepath.Join(dir, "app")), gitInit(t, filepath.Join(dir, "web"))
	forges := map[string]*fakePRForge{
		app: {prs: []forge.PRDetail{{Number: 3, Title: "old app pr", Author: "ana"}}},
		web: {prs: []forge.PRDetail{{Number: 8, Title: "web pr", Author: "bo"}}},
	}
	byRepo := func(_ context.Context, repo string) (forge.Forge, error) { return forges[repo], nil }
	m := send(t, startReviewTUI(t, &fakeWatchActions{}, nil, []string{app, web}, &now, byRepo), keyPress("r"))
	m, check := m.Update(keyPress("enter"))
	m, load := m.Update(check())
	if load == nil {
		t.Fatalf("the PRs of app load:\n%s", screen(m))
	}
	late := load() // the PRs of app, answered after the user moved on

	m = send(t, send(t, send(t, m, keyPress("esc")), keyPress("j")), keyPress("enter"))
	m = send(t, m, late)
	if text := reviewText(m); !pickerHas(text, "Which PR in web?") || !pickerHas(text, "#8 web pr") || pickerHas(text, "#3") {
		t.Fatalf("the PRs of app, loaded late, do not replace those of web:\n%s", screen(m))
	}

	m = send(t, m, keyPress("esc"))
	m, check = m.Update(keyPress("enter")) // app again, then the dialog closes before its PRs come
	m, load = m.Update(check())
	m = send(t, send(t, m, keyPress("esc")), keyPress("esc"))
	if m = send(t, m, load()); strings.Contains(screen(m), "delamain review") {
		t.Fatalf("the PRs of a closed dialog open nothing:\n%s", screen(m))
	}
}

func TestWatchStartReviewShouldToastTheExitOfTheReview(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{startReviewExit{pr: 12, code: 1, log: "/s/launch/1767225600-review-12.log"}, "review of PR #12 failed: exit 1; see /s/launch/1767225600-review-12.log"},
		{errors.New("fork/exec /opt/delamain: no such file or directory"), "fork/exec /opt/delamain: no such file or directory"},
		// The log is under the state dir, which could hold anything: it reaches no terminal raw.
		{startReviewExit{pr: 12, code: 1, log: "/s/\x1b]52;c;ZXZpbA==\x07x.log"}, "review of PR #12 failed: exit 1; see /s/]52;c;ZXZpbA==x.log"},
	} {
		now := watchNow
		acts := &fakeWatchActions{reviewErr: c.err}
		var asked []string
		m, _ := pickApp(t, acts, &now, prForgeOf(twoPRs(), &asked))
		m = send(t, m, keyPress("enter"))
		afterGrace(&now)
		m = send(t, m, keyPress("enter"))
		if got := strings.Join(footerText(m)[:len(footerText(m))-1], " "); got != c.want {
			t.Errorf("%v is the toast %q, want %q", c.err, got, c.want)
		}
		if strings.Contains(m.View(), "\x1b]52") {
			t.Errorf("the toast of %q writes no escape of its own", c.err)
		}
	}
}

// runOn is an active run of kind on repo, for issue ("pr:9", "addr:9" or "#9").
func runOn(id int64, kind, repo, issue string) RunView {
	v := owned(id, issue, "a", 51230+int(id), true)
	v.Kind, v.Repo = kind, repo
	return v
}

func TestWatchStartReviewShouldBlockPRWithActiveRun(t *testing.T) {
	now := watchNow
	app := gitInit(t, filepath.Join(t.TempDir(), "app"))
	other := filepath.Join(filepath.Dir(app), "other")
	src := &fakeWatchSource{active: []RunView{
		owned(14, "#55", "a", 51230, true),
		runOn(20, "review", app+"/", "pr:12"), // the same repository, written otherwise
		runOn(21, "address", app, "addr:9"),
		runOn(22, "review", other, "pr:7"), // another repository
		runOn(23, "issue", app, "#8"),      // an issue run, not a PR
	}}
	f := &fakePRForge{prs: []forge.PRDetail{
		{Number: 12, Title: "fix the parser", Author: "ana"},
		{Number: 9, Title: "wip: watch", Author: "bo"},
		{Number: 8, Title: "eight", Author: "cy"},
		{Number: 7, Title: "seven", Author: "di"},
	}}
	var asked []string
	acts := &fakeWatchActions{}
	m := send(t, send(t, startReviewTUI(t, acts, src, []string{app}, &now, prForgeOf(f, &asked)), keyPress("r")), keyPress("enter"))
	text := reviewText(m)
	for _, want := range []string{"#12 fix the parser @ana active run", "#9 wip: watch @bo active run", "#8 eight @cy", "#7 seven @di"} {
		if !pickerHas(text, want) {
			t.Fatalf("the list marks the PRs with an active run, %q:\n%s", want, screen(m))
		}
	}
	if pickerHas(text, "#8 eight @cy active run") || pickerHas(text, "#7 seven @di active run") {
		t.Fatalf("an issue run, or a review of another repository, marks no PR:\n%s", screen(m))
	}

	afterGrace(&now)
	for _, k := range []string{"", "j"} { // #12 has a review, #9 an address
		if k != "" {
			m = send(t, m, keyPress(k))
		}
		m = send(t, m, keyPress("enter"))
		pr := map[string]string{"": "12", "j": "9"}[k]
		if got := footerText(m)[0]; got != "a run is already active for PR "+pr {
			t.Fatalf("enter on PR %s says a run is active: %q", pr, got)
		}
		if text := reviewText(m); !pickerHas(text, "Which PR in app?") || pickerHas(text, "Review PR #") {
			t.Fatalf("the list stays, with no confirmation:\n%s", screen(m))
		}
	}

	// A review started after the list loaded is seen at the next reload.
	src.active = append(src.active, runOn(24, "review", app, "pr:8"))
	m = send(t, reload(t, m), keyPress("j"))
	if m = send(t, m, keyPress("enter")); footerText(m)[0] != "a run is already active for PR 8" {
		t.Fatalf("enter checks the runs of the last reload:\n%s", screen(m))
	}
	m = send(t, send(t, m, keyPress("j")), keyPress("enter"))
	if !pickerHas(reviewText(m), "Review PR #7 (seven) in app?") {
		t.Fatalf("a PR without an active run of this repository can be reviewed:\n%s", screen(m))
	}
	afterGrace(&now)
	if send(t, m, keyPress("enter")); len(acts.reviewed) != 1 || acts.reviewed[0] != (reviewCall{app, 7}) {
		t.Fatalf("only PR 7 is reviewed: %v", acts.reviewed)
	}
}
