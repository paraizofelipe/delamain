package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/store"
)

// removeCall is one call of fakeWatchActions.Remove.
type removeCall struct {
	id    int64
	force bool
}

// fakeWatchActions records what the TUI asks of a run and answers what each test sets.
type fakeWatchActions struct {
	mu        sync.Mutex
	stops     []int64
	removes   []removeCall
	opened    []string
	copied    []string
	stopOut   string
	stopErr   error
	removeOut string
	removeErr []error // one per call, nil once used up
	openErr   error
	copyErr   error
}

func (f *fakeWatchActions) Stop(_ context.Context, id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, id)
	return f.stopOut, f.stopErr
}

func (f *fakeWatchActions) Remove(_ context.Context, id int64, force bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, removeCall{id, force})
	var err error
	if len(f.removeErr) > 0 {
		err, f.removeErr = f.removeErr[0], f.removeErr[1:]
	}
	if err != nil {
		return "", err
	}
	return f.removeOut, nil
}

func (f *fakeWatchActions) OpenURL(url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, url)
	return f.openErr
}

func (f *fakeWatchActions) Copy(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copied = append(f.copied, text)
	return f.copyErr
}

// startActions is the TUI with acts after its first window size and first reload; the clock
// reads *now.
func startActions(t *testing.T, src *fakeWatchSource, acts *fakeWatchActions, width, height int, now *time.Time) tea.Model {
	t.Helper()
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return *now }, actions: acts}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	return settle(t, m, m.Init())
}

// owned is an active run whose owner has pid and is alive or not.
func owned(id int64, issue, title string, pid int, alive bool) RunView {
	v := active(id, issue, "implement", "low", title)
	v.Owner = Process{PID: pid, Alive: alive}
	return v
}

// quits reports whether cmd ends the program.
func quits(cmd tea.Cmd) bool {
	for _, msg := range messages(cmd) {
		if _, ok := msg.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

// dialogText is the text inside the dialog box titled title, spaces collapsed, or "".
func dialogText(view, title string) string {
	return words(panelText(view, title))
}

func TestWatchStopShouldAskAndThenStopOnlyTheSelectedRun(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{
		owned(14, "#55", "artifacts: accept test_file", 51230, true),
		owned(15, "#54", "tui: delamain watch", 51231, true),
	}}
	acts := &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n"}
	m := startActions(t, src, acts, 160, 40, &now)

	m = send(t, m, keyPress("s"))
	text := dialogText(screen(m), "delamain stop")
	for _, want := range []string{
		"Stop run 14?", "#55 artifacts: accept test_file",
		"owner pid 51230 (alive): only this run is cancelled",
		"result halted: stopped by the user (delamain stop)",
		"delivery draft of what was already committed + needs-human label",
		"[ stop s ] [ cancel esc ]",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the stop dialog says %q:\n%s", want, screen(m))
		}
	}
	// With the dialog open only its keys act, q does not quit and the reloads go on.
	reads := src.reads
	for _, k := range []string{"j", "z", "q"} {
		next, cmd := m.Update(keyPress(k))
		if quits(cmd) {
			t.Fatalf("%s must not quit with the dialog open", k)
		}
		m = settle(t, next, cmd)
	}
	m = reload(t, m)
	if src.reads == reads {
		t.Fatal("the reloads go on behind the dialog")
	}
	if view := screen(m); !strings.Contains(view, "╭ status · run 14 ") || !strings.Contains(view, "delamain stop") {
		t.Fatalf("the keys of the screen do nothing behind the dialog:\n%s", view)
	}

	for _, cancel := range []string{"esc", "n"} {
		m = send(t, send(t, m, keyPress(cancel)), keyPress("s"))
		m = send(t, m, keyPress(cancel))
		if strings.Contains(screen(m), "delamain stop") {
			t.Fatalf("%s closes the dialog:\n%s", cancel, screen(m))
		}
	}
	if len(acts.stops) != 0 {
		t.Fatalf("cancelling stops nothing: %v", acts.stops)
	}

	for _, confirm := range []string{"s", "enter"} {
		acts.stops = nil
		m = send(t, m, keyPress("s"))
		next, cmd := m.Update(keyPress(confirm))
		if view := screen(next); strings.Contains(view, "delamain stop") || footerText(next)[0] != "stopping run 14…" {
			t.Fatalf("%s closes the dialog and the footer says the stop is running:\n%s", confirm, view)
		}
		if len(acts.stops) != 0 {
			t.Fatal("the stop runs in a tea.Cmd, off Update")
		}
		again, _ := next.Update(keyPress("s"))
		if _, cmd := again.Update(keyPress(confirm)); cmd != nil {
			t.Fatal("a stop waits for the running one to end")
		}
		m = settle(t, next, cmd)
		if len(acts.stops) != 1 || acts.stops[0] != 14 {
			t.Fatalf("%s stops only the selected run, once: %v", confirm, acts.stops)
		}
		if got := strings.Join(footerText(m), "\n"); !strings.Contains(got, "stop requested for run 14 (owner pid 51230)") {
			t.Fatalf("what stop prints is the toast: %q", got)
		}
	}

	acts.stopErr = errors.New("run 14: database is locked")
	m = send(t, send(t, m, keyPress("s")), keyPress("s"))
	if got := strings.Join(footerText(m), "\n"); !strings.Contains(got, "run 14: database is locked") {
		t.Fatalf("a failed stop is the toast: %q", got)
	}
}

// footerText is the footer of m, without colors.
func footerText(m tea.Model) []string {
	var out []string
	for _, l := range m.(watchModel).footer() {
		out = append(out, strings.TrimSpace(stripANSI(l)))
	}
	return out
}

func TestWatchToastShouldDisappearAfter5Seconds(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	acts := &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n"}
	m := startActions(t, src, acts, 160, 40, &now)
	keys := "↑↓ navigate  tab focus  1-6 source"

	// The stop that runs keeps its toast as long as it runs.
	m = send(t, m, keyPress("s"))
	m, cmd := m.Update(keyPress("s"))
	now = now.Add(time.Minute)
	m = reload(t, m)
	if got := footerText(m); got[0] != "stopping run 14…" {
		t.Fatalf("the toast of a running stop stays: %q", got)
	}
	m = settle(t, m, cmd)
	got := footerText(m)
	if got[0] != "stop requested for run 14 (owner pid 51230)" || !strings.HasPrefix(got[1], keys) {
		t.Fatalf("the toast takes the first footer line and pushes the keys down: %q", got)
	}

	now = now.Add(4 * time.Second)
	if m = reload(t, m); footerText(m)[0] != "stop requested for run 14 (owner pid 51230)" {
		t.Fatalf("the toast stays for 5s: %q", footerText(m))
	}
	// A new toast replaces the old one and has its own 5s.
	acts.stopOut = "run 14 already finished (recorded)\n"
	m = send(t, send(t, m, keyPress("s")), keyPress("s"))
	if got := footerText(m); got[0] != "run 14 already finished (recorded)" || !strings.HasPrefix(got[1], keys) {
		t.Fatalf("a new toast replaces the old one: %q", got)
	}
	now = now.Add(2 * time.Second)
	if m = reload(t, m); footerText(m)[0] != "run 14 already finished (recorded)" {
		t.Fatalf("the new toast does not go with the old one's time: %q", footerText(m))
	}
	now = now.Add(3 * time.Second)
	m = reload(t, m)
	if got := footerText(m); len(got) != 1 || !strings.HasPrefix(got[0], keys) {
		t.Fatalf("the toast is gone after 5s and the keys come back up: %q", got)
	}
}

func TestWatchStopShouldSayWhenTheRunAlreadyFinished(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{done: []RunView{finished(14, "#55", "approved: merged", "artifacts: accept test_file")}}
	acts := &fakeWatchActions{}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("s"))
	text := dialogText(screen(m), "delamain stop")
	for _, want := range []string{"run 14 already finished", "state recorded", "result approved: merged", "[ ok esc ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("a finished run's dialog says %q:\n%s", want, screen(m))
		}
	}
	for _, unwanted := range []string{"Stop run", "[ stop s ]", "delivery"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("a finished run has nothing to stop, but the dialog offers %q:\n%s", unwanted, text)
		}
	}
	for _, k := range []string{"s", "enter"} {
		if m = send(t, m, keyPress(k)); dialogText(screen(m), "delamain stop") == "" {
			t.Fatalf("%s does not close the dialog of a finished run", k)
		}
	}
	if m = send(t, m, keyPress("esc")); strings.Contains(screen(m), "delamain stop") {
		t.Fatalf("esc closes it:\n%s", screen(m))
	}
	if len(acts.stops) != 0 {
		t.Fatalf("a finished run is never stopped: %v", acts.stops)
	}
}

func TestWatchStopShouldOfferRecoverWhenTheOwnerDied(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 999999, false)}}
	acts := &fakeWatchActions{stopOut: "run 14 recovered:\n❌ halted — halted: run interrompida\n   report:     /r/report.md\n"}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("s"))
	text := dialogText(screen(m), "delamain stop")
	if want := "owner pid 999999 (dead): the run is finalized here, as delamain recover does"; !strings.Contains(text, want) {
		t.Fatalf("a dead owner's dialog says %q:\n%s", want, screen(m))
	}
	if strings.Contains(text, "only this run is cancelled") {
		t.Fatalf("nobody is left to cancel the run:\n%s", text)
	}
	m = send(t, m, keyPress("enter"))
	if len(acts.stops) != 1 || acts.stops[0] != 14 {
		t.Fatalf("the confirmation stops the run, which takes the recover path: %v", acts.stops)
	}
	if got := footerText(m)[0]; got != "run 14 recovered: · ❌ halted — halted: run interrompida · report: /r/report.md" {
		t.Fatalf("the lines recover prints are one toast: %q", got)
	}
}

// actions are the real actions of `watch` on the fixture's store; out is the terminal.
func (f *fixture) actions(t *testing.T, out *bytes.Buffer) *storeWatchActions {
	t.Helper()
	cfg, err := config.Load(f.cfgFile, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	acts := newStoreWatchActions(cfg, f.st, out)
	t.Cleanup(acts.close)
	return acts
}

func TestWatchActionsShouldStopARunLikeStopNoWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "", store.StateImplement) // owned by this test process, which is alive
	done := f.run(t, "#2", "", store.StateRecorded)
	acts := f.actions(t, &bytes.Buffer{})

	start := time.Now()
	out, err := acts.Stop(ctx, rec.ID)
	if want := "stop requested for run 1 (owner pid " + strconv.Itoa(os.Getpid()) + ")\n"; err != nil || out != want {
		t.Fatalf("Stop prints what stop --no-wait prints, want %q: %q %v", want, out, err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("Stop never waits for the run")
	}
	if !f.stopRequested(t, rec.ID) {
		t.Fatal("the request is recorded for the owner to see")
	}
	if got, _ := f.st.GetRun(ctx, rec.ID); got.State != store.StateImplement {
		t.Fatalf("a live owner's run is finished by its owner, not by Stop: %+v", got)
	}

	if out, err := acts.Stop(ctx, done.ID); err != nil || out != "run 2 already finished (recorded)\n" {
		t.Fatalf("a finished run is left as it is: %q %v", out, err)
	}
	if f.stopRequested(t, done.ID) {
		t.Fatal("a finished run gets no stop request")
	}
	if _, err := acts.Stop(ctx, 999); err == nil || err.Error() != "run 999 not found" {
		t.Fatalf("an unknown run is an error: %v", err)
	}
}

func TestWatchActionsShouldRecoverARunWhoseOwnerDied(t *testing.T) {
	n := &fakeNotifier{}
	useFakeNotifier(t, n, nil)
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement)
	other := f.run(t, "#2", "fix/2-b", store.StateImplement)
	for _, id := range []int64{rec.ID, other.ID} {
		if ok, err := f.st.ClaimRun(ctx, id, os.Getpid(), deadPID); err != nil || !ok {
			t.Fatal("seed a run whose process died", err)
		}
	}
	acts := f.actions(t, &bytes.Buffer{})

	out, err := acts.Stop(ctx, rec.ID)
	if err != nil || !strings.HasPrefix(out, "run 1 recovered:\n") || !strings.Contains(out, "halted") {
		t.Fatalf("Stop prints the result as recover does: %q %v", out, err)
	}
	got, err := f.st.GetRun(ctx, rec.ID)
	if err != nil || got.State != store.StateRecorded || !strings.HasPrefix(got.Reason, "halted: run interrompida") {
		t.Fatalf("the orphan is finalized as recover would: %+v %v", got, err)
	}
	if got, _ := f.st.GetRun(ctx, other.ID); got.State != store.StateImplement {
		t.Fatalf("only the given run is recovered: %+v", got)
	}
	if ev := n.events(); len(ev) != 1 || ev[0] != notify.EventRecover {
		t.Fatalf("the recover toast is sent as recover sends it: %v", ev)
	}
	acts.close()
	if !n.closed {
		t.Fatal("closing the actions closes the notifier of the recover")
	}
}

func TestWatchRmShouldRefuseActiveRuns(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	acts := &fakeWatchActions{}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("d"))
	text := dialogText(screen(m), "delamain rm")
	for _, want := range []string{"run 14 is still implement", "stop it first with s: rm only deletes finished runs", "[ ok esc ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("an active run's rm dialog says %q:\n%s", want, screen(m))
		}
	}
	if strings.Contains(text, "Remove run") || strings.Contains(text, "[ remove d ]") {
		t.Fatalf("an active run is never offered for removal:\n%s", text)
	}
	for _, k := range []string{"d", "enter", "F"} {
		if m = send(t, m, keyPress(k)); dialogText(screen(m), "delamain rm") == "" {
			t.Fatalf("%s does not close the refusal", k)
		}
	}
	if m = send(t, m, keyPress("esc")); strings.Contains(screen(m), "delamain rm") {
		t.Fatalf("esc closes it:\n%s", screen(m))
	}
	if len(acts.removes) != 0 {
		t.Fatalf("an active run is never removed: %v", acts.removes)
	}
}

// removable is a finished run with a branch, as rm finds it.
func removable(id int64) RunView {
	v := finished(id, "#49", "approved: merged", "logs: colors by source")
	v.Branch = "feat/49-logs-colors"
	return v
}

func TestWatchRmShouldOfferForceOnADirtyWorktree(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{done: []RunView{removable(12)}}
	dirty := "worktree /wt/app/feat/49-logs-colors has uncommitted changes; use --force to discard them"
	acts := &fakeWatchActions{removeOut: "removed run 12\n", removeErr: []error{
		fmt.Errorf("worktree /wt/app/feat/49-logs-colors %w", errDirtyWorktree),
	}}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("d"))
	text := dialogText(screen(m), "delamain rm")
	for _, want := range []string{
		"Remove run 12?", "#49 logs: colors by source",
		"deletes record, run directory and worktree",
		"keeps branch feat/49-logs-colors and the PR",
		"budget the day's spend does not change (token_ledger stays)",
		"[ remove d ] [ cancel esc ]",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the rm dialog says %q:\n%s", want, screen(m))
		}
	}
	if m = send(t, m, keyPress("F")); len(acts.removes) != 0 {
		t.Fatalf("F is only offered after a dirty worktree: %v", acts.removes)
	}

	m = send(t, m, keyPress("d"))
	if len(acts.removes) != 1 || acts.removes[0] != (removeCall{12, false}) {
		t.Fatalf("d removes the run without force: %v", acts.removes)
	}
	text = dialogText(screen(m), "delamain rm")
	if !strings.Contains(text, dirty) || !strings.Contains(text, "[ remove anyway F ]") {
		t.Fatalf("a dirty worktree keeps the dialog open with the error and F:\n%s", screen(m))
	}

	m = send(t, m, keyPress("F"))
	if len(acts.removes) != 2 || acts.removes[1] != (removeCall{12, true}) {
		t.Fatalf("F removes with force: %v", acts.removes)
	}
	if strings.Contains(screen(m), "delamain rm") || footerText(m)[0] != "removed run 12" {
		t.Fatalf("the removal closes the dialog and is the toast: %q\n%s", footerText(m), screen(m))
	}
}

func TestWatchRmShouldRemoveAFinishedRunAndWarnOnOtherErrors(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{done: []RunView{removable(12)}}
	acts := &fakeWatchActions{removeOut: "removed run 12\n"}
	m := startActions(t, src, acts, 160, 40, &now)

	m = send(t, send(t, m, keyPress("d")), keyPress("esc"))
	m = send(t, send(t, m, keyPress("d")), keyPress("n"))
	if len(acts.removes) != 0 || strings.Contains(screen(m), "delamain rm") {
		t.Fatalf("esc and n cancel: %v", acts.removes)
	}

	m = send(t, send(t, m, keyPress("d")), keyPress("enter"))
	if len(acts.removes) != 1 || acts.removes[0] != (removeCall{12, false}) || footerText(m)[0] != "removed run 12" {
		t.Fatalf("enter removes the run and says so: %v %q", acts.removes, footerText(m))
	}
	if strings.Contains(screen(m), "delamain rm") {
		t.Fatalf("the dialog closes:\n%s", screen(m))
	}

	acts.removeErr = []error{errors.New("worktree /wt/x is in use by run 2 (implement)")}
	m = send(t, send(t, m, keyPress("d")), keyPress("d"))
	if strings.Contains(screen(m), "delamain rm") || footerText(m)[0] != "worktree /wt/x is in use by run 2 (implement)" {
		t.Fatalf("another error closes the dialog and is the toast: %q\n%s", footerText(m), screen(m))
	}
}

func TestWatchActionsShouldRemoveARunLikeRm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded)
	dir := f.runDir(t, rec)
	busy := f.run(t, "#2", "", store.StateImplement)
	acts := f.actions(t, &bytes.Buffer{})

	if out, err := acts.Remove(ctx, rec.ID, false); err != nil || out != "removed run 1\n" {
		t.Fatalf("Remove prints what rm prints: %q %v", out, err)
	}
	if _, err := f.st.GetRun(ctx, rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the record is gone, got %v", err)
	}
	if exists(dir) || exists(rec.Worktree) {
		t.Fatal("the run directory and the worktree are gone")
	}
	if !f.branchExists("fix/1-a") {
		t.Fatal("the branch may be the delivered draft: it stays")
	}

	_, err := acts.Remove(ctx, busy.ID, true)
	if want := "run 2 is still implement: stop it first with 'delamain stop 2'"; err == nil || err.Error() != want {
		t.Fatalf("an unfinished run is refused, even forced, want %q: %v", want, err)
	}
	if _, err := f.st.GetRun(ctx, busy.ID); err != nil {
		t.Fatalf("a refused run keeps its record: %v", err)
	}
	if _, err := acts.Remove(ctx, 999, false); err == nil || err.Error() != "run 999 not found" {
		t.Fatalf("an unknown run is an error: %v", err)
	}
}

func TestWatchActionsShouldRefuseADirtyWorktreeWithErrDirtyWorktreeUnlessForced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded)
	dir := f.runDir(t, rec)
	if err := os.WriteFile(filepath.Join(rec.Worktree, "wip.go"), []byte("package wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acts := f.actions(t, &bytes.Buffer{})

	_, err := acts.Remove(ctx, rec.ID, false)
	if !errors.Is(err, errDirtyWorktree) {
		t.Fatalf("a dirty worktree is errDirtyWorktree: %v", err)
	}
	if want := "worktree " + rec.Worktree + " has uncommitted changes; use --force to discard them"; err.Error() != want {
		t.Fatalf("the message is rm's, want %q: %q", want, err)
	}
	if !exists(filepath.Join(rec.Worktree, "wip.go")) || !exists(dir) {
		t.Fatal("a refused run keeps its worktree and directory")
	}

	if out, err := acts.Remove(ctx, rec.ID, true); err != nil || out != "removed run 1\n" {
		t.Fatalf("force discards the changes: %q %v", out, err)
	}
	if exists(rec.Worktree) || exists(dir) {
		t.Fatal("force removes the worktree and the run directory")
	}
}

func TestWatchOpenShouldOpenThePRURLOrWarnWithoutOne(t *testing.T) {
	now := watchNow
	withPR := owned(14, "#55", "a", 51230, true)
	withPR.PR = "https://github.com/o/r/pull/7"
	src := &fakeWatchSource{active: []RunView{withPR, owned(15, "#56", "b", 51230, true)}}
	acts := &fakeWatchActions{}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("o"))
	if len(acts.opened) != 1 || acts.opened[0] != "https://github.com/o/r/pull/7" {
		t.Fatalf("o opens the selected run's PR: %v", acts.opened)
	}
	if got := footerText(m); len(got) != 1 {
		t.Fatalf("an opened PR needs no toast: %q", got)
	}

	acts.openErr = errors.New(`exec: "xdg-open": executable file not found in $PATH`)
	if m = send(t, m, keyPress("o")); footerText(m)[0] != `exec: "xdg-open": executable file not found in $PATH` {
		t.Fatalf("a failed open is the toast: %q", footerText(m))
	}

	m = send(t, send(t, m, keyPress("j")), keyPress("o"))
	if len(acts.opened) != 2 || footerText(m)[0] != "run 15 has no PR/MR" {
		t.Fatalf("a run without a PR opens nothing and says so: %v %q", acts.opened, footerText(m))
	}
}

func TestWatchActionsShouldOpenOnlyHTTPSURLs(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(dir, "opened")
	opener := filepath.Join(dir, "opener")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nprintf '%s' \"$*\" > "+got+"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	acts := &storeWatchActions{opener: opener}
	for _, bad := range []string{
		"file:///etc/passwd", "http://github.com/o/r/pull/7", "https://user:pw@github.com/o/r/pull/7",
		"-https://github.com", "https:///o/r", "javascript:alert(1)", "/usr/share/applications/x.desktop", "",
	} {
		if err := acts.OpenURL(bad); err == nil {
			t.Fatalf("%q is refused", bad)
		}
	}
	if exists(got) {
		t.Fatal("a refused URL starts no process")
	}

	url := "https://gitlab.example.com/g/r/-/merge_requests/7"
	if err := acts.OpenURL(url); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(got); string(b) == url { //nolint:gosec // inside the test's temp dir
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the opener gets the URL as its only argument")
		}
		time.Sleep(10 * time.Millisecond)
	}

	acts.opener = filepath.Join(dir, "missing")
	if err := acts.OpenURL(url); err == nil {
		t.Fatal("an opener that cannot start is an error")
	}
}
