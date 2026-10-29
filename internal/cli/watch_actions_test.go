package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

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
	mu         sync.Mutex
	stops      []int64
	removes    []removeCall
	opened     []string
	copied     []string
	stopOut    string
	stopErr    error
	removeOut  string
	removeErr  []error // one per call, nil once used up
	openErr    error
	copyErr    error
	recovers   []int64
	recoverOut string
	recoverErr error
	started    []startCall
	startErr   error
}

// startCall is one call of fakeWatchActions.StartIssue.
type startCall struct {
	repo  string
	issue int
}

func (f *fakeWatchActions) StartIssue(repo string, issue int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, startCall{repo, issue})
	return f.startErr
}

func (f *fakeWatchActions) Stop(id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, id)
	return f.stopOut, f.stopErr
}

func (f *fakeWatchActions) Remove(id int64, force bool) (string, error) {
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

func (f *fakeWatchActions) Recover(id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovers = append(f.recovers, id)
	return f.recoverOut, f.recoverErr
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
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return *now }, actions: acts, after: noTimers}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: width, Height: height})
	return settle(t, m, m.Init())
}

// noTimers drops the toast timers: the toasts of those tests stay.
func noTimers(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }

// afterGrace moves the clock past confirmGrace, as a user who read the dialog before answering
// it: until then rm ignores the key that opened it.
func afterGrace(now *time.Time) { *now = now.Add(confirmGrace) }

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
		afterGrace(&now)
		next, cmd := m.Update(keyPress(confirm))
		if view := screen(next); strings.Contains(view, "delamain stop") || footerText(next)[0] != "stopping run 14…" {
			t.Fatalf("%s closes the dialog and the footer says the stop is running:\n%s", confirm, view)
		}
		if len(acts.stops) != 0 {
			t.Fatal("the stop runs in a tea.Cmd, off Update")
		}
		again, _ := next.Update(keyPress("s"))
		afterGrace(&now) // what holds this stop back is the running one, not the grace
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
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	m = send(t, m, keyPress("s"))
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

// fakeTimers are the toast timers of a test: they fire when the test moves the clock past them.
type fakeTimers struct {
	now *time.Time
	due []fakeTimer
}

type fakeTimer struct {
	at time.Time
	fn func(time.Time) tea.Msg
}

func (f *fakeTimers) after(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	f.due = append(f.due, fakeTimer{f.now.Add(d), fn})
	return nil
}

// elapse moves the clock d ahead and delivers the timers that are due.
func (f *fakeTimers) elapse(t *testing.T, m tea.Model, d time.Duration) tea.Model {
	t.Helper()
	*f.now = f.now.Add(d)
	var left []fakeTimer
	due := f.due
	f.due = nil
	for _, tm := range due {
		if tm.at.After(*f.now) {
			left = append(left, tm)
			continue
		}
		m = send(t, m, tm.fn(*f.now))
	}
	f.due = append(left, f.due...)
	return m
}

func TestWatchToastShouldDisappearAfter5Seconds(t *testing.T) {
	now := watchNow
	timers := &fakeTimers{now: &now}
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	acts := &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n"}
	// No reload tick is delivered here: the toast goes on its own 5s, whatever --interval is.
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return now }, actions: acts, after: timers.after}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m = settle(t, m, m.Init())
	keys := "↑↓ navigate  tab focus  1-6 source"

	// The stop that runs keeps its toast as long as it runs.
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	m, cmd := m.Update(keyPress("s"))
	if m = timers.elapse(t, m, time.Minute); footerText(m)[0] != "stopping run 14…" {
		t.Fatalf("the toast of a running stop stays: %q", footerText(m))
	}
	m = settle(t, m, cmd)
	got := footerText(m)
	if got[0] != "stop requested for run 14 (owner pid 51230)" || !strings.HasPrefix(got[1], keys) {
		t.Fatalf("the toast takes the first footer line and pushes the keys down: %q", got)
	}

	if m = timers.elapse(t, m, 4*time.Second); footerText(m)[0] != "stop requested for run 14 (owner pid 51230)" {
		t.Fatalf("the toast stays for 5s: %q", footerText(m))
	}
	// A new toast replaces the old one and has its own 5s.
	acts.stopOut = "run 14 already finished (recorded)\n"
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	m = send(t, m, keyPress("s"))
	if got := footerText(m); got[0] != "run 14 already finished (recorded)" || !strings.HasPrefix(got[1], keys) {
		t.Fatalf("a new toast replaces the old one: %q", got)
	}
	if m = timers.elapse(t, m, 2*time.Second); footerText(m)[0] != "run 14 already finished (recorded)" {
		t.Fatalf("the new toast does not go with the old one's time: %q", footerText(m))
	}
	m = timers.elapse(t, m, 3*time.Second)
	if got := footerText(m); len(got) != 1 || !strings.HasPrefix(got[0], keys) {
		t.Fatalf("the toast is gone 5s after it showed, with no reload in between: %q", got)
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
	afterGrace(&now)
	m = send(t, m, keyPress("enter"))
	if len(acts.stops) != 1 || acts.stops[0] != 14 {
		t.Fatalf("the confirmation stops the run, which takes the recover path: %v", acts.stops)
	}
	if got := footerText(m)[0]; got != "run 14 recovered: · ❌ halted — halted: run interrompida · report: /r/report.md" {
		t.Fatalf("the lines recover prints are one toast: %q", got)
	}
}

// actions are the real actions of `watch` on the fixture's store, under ctx as the command
// gives it; out is the terminal.
func (f *fixture) actions(t *testing.T, ctx context.Context, out *bytes.Buffer) *storeWatchActions { //nolint:revive // ctx after t is the order of the other helpers
	t.Helper()
	cfg, err := config.Load(f.cfgFile, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	acts := newStoreWatchActions(ctx, cfg, f.st, out)
	t.Cleanup(acts.close)
	return acts
}

func TestWatchActionsShouldStopARunLikeStopNoWait(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "", store.StateImplement) // owned by this test process, which is alive
	done := f.run(t, "#2", "", store.StateRecorded)
	acts := f.actions(t, ctx, &bytes.Buffer{})

	// What proves Stop never waits is the message and the nil error: the stopper of `watch` has
	// no timeout, so a Stop that waited would fail in waitStopped instead of printing this.
	out, err := acts.Stop(rec.ID)
	if want := "stop requested for run 1 (owner pid " + strconv.Itoa(os.Getpid()) + ")\n"; err != nil || out != want {
		t.Fatalf("Stop prints what stop --no-wait prints, want %q: %q %v", want, out, err)
	}
	if !f.stopRequested(t, rec.ID) {
		t.Fatal("the request is recorded for the owner to see")
	}
	if got, _ := f.st.GetRun(ctx, rec.ID); got.State != store.StateImplement {
		t.Fatalf("a live owner's run is finished by its owner, not by Stop: %+v", got)
	}

	if out, err := acts.Stop(done.ID); err != nil || out != "run 2 already finished (recorded)\n" {
		t.Fatalf("a finished run is left as it is: %q %v", out, err)
	}
	if f.stopRequested(t, done.ID) {
		t.Fatal("a finished run gets no stop request")
	}
	if _, err := acts.Stop(999); err == nil || err.Error() != "run 999 not found" {
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
	acts := f.actions(t, ctx, &bytes.Buffer{})

	out, err := acts.Stop(rec.ID)
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

func TestWatchRecoverShouldOpenDialogForOrphanedRun(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "artifacts: accept test_file", 999999, false)}}
	acts := &fakeWatchActions{}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("c"))
	text := dialogText(screen(m), "delamain recover")
	for _, want := range []string{
		"Recover run 14?", "#55 artifacts: accept test_file",
		"owner pid 999999 (dead)",
		"agents stopped",
		"delivery draft of what was already committed; empty work is removed",
		"result halted: the run was interrupted",
		"[ recover c ] [ cancel esc ]",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the recover dialog says %q:\n%s", want, screen(m))
		}
	}
	if len(acts.recovers) != 0 {
		t.Fatalf("opening the dialog recovers nothing: %v", acts.recovers)
	}
}

func TestWatchRecoverShouldCallRecoverOnConfirm(t *testing.T) {
	src := &fakeWatchSource{active: []RunView{
		owned(14, "#55", "a", 999998, false),
		owned(15, "#56", "b", 999999, false),
	}}
	for _, confirm := range []string{"c", "enter"} {
		now := watchNow
		acts := &fakeWatchActions{recoverOut: "run 15 recovered:\n❌ halted — halted: run interrompida\n   report:     /r/report.md\n"}
		m := send(t, send(t, startActions(t, src, acts, 160, 40, &now), keyPress("j")), keyPress("c"))
		afterGrace(&now)
		next, cmd := m.Update(keyPress(confirm))
		if view := screen(next); dialogText(view, "delamain recover") != "" || footerText(next)[0] != "recovering run 15…" {
			t.Fatalf("%s closes the dialog and the footer says the recover is running:\n%s", confirm, view)
		}
		if len(acts.recovers) != 0 {
			t.Fatal("the recover runs in a tea.Cmd, off Update")
		}
		m = settle(t, next, cmd)
		if len(acts.recovers) != 1 || acts.recovers[0] != 15 {
			t.Fatalf("%s recovers only the selected run, once: %v", confirm, acts.recovers)
		}
		if len(acts.stops) != 0 || len(acts.removes) != 0 {
			t.Fatalf("%s neither stops nor removes: %+v", confirm, acts)
		}
		if got := footerText(m)[0]; got != "run 15 recovered: · ❌ halted — halted: run interrompida · report: /r/report.md" {
			t.Fatalf("what recover prints is the toast: %q", got)
		}
	}
}

func TestWatchRecoverShouldIgnoreConfirmWithinGrace(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{recoverOut: "run 14 recovered:\n"}
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 999999, false)}}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("c"))
	for _, k := range []string{"c", "enter"} {
		next, cmd := m.Update(keyPress(k))
		if cmd != nil || len(acts.recovers) != 0 {
			t.Fatalf("%s right after the dialog opened is a double tap, not a choice", k)
		}
		if m = settle(t, next, cmd); dialogText(screen(m), "delamain recover") == "" {
			t.Fatalf("%s leaves the dialog open to be read:\n%s", k, screen(m))
		}
	}
	now = now.Add(confirmGrace - time.Millisecond)
	if m = send(t, m, keyPress("enter")); len(acts.recovers) != 0 {
		t.Fatalf("the grace is %s, not less", confirmGrace)
	}
	afterGrace(&now)
	send(t, m, keyPress("enter"))
	if len(acts.recovers) != 1 {
		t.Fatalf("enter recovers once the dialog could be read: %v", acts.recovers)
	}
}

func TestWatchRecoverShouldNotCallRecoverOnCancel(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 999999, false)}}
	m := startActions(t, src, acts, 160, 40, &now)
	for _, cancel := range []string{"esc", "n"} {
		m = send(t, m, keyPress("c"))
		afterGrace(&now)
		if m = send(t, m, keyPress(cancel)); dialogText(screen(m), "delamain recover") != "" {
			t.Fatalf("%s closes the dialog:\n%s", cancel, screen(m))
		}
	}
	if len(acts.recovers) != 0 {
		t.Fatalf("cancelling recovers nothing: %v", acts.recovers)
	}
}

func TestWatchRecoverShouldToastWhenOwnerIsAlive(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("c"))
	if dialogText(screen(m), "delamain recover") != "" {
		t.Fatalf("a run whose owner is alive is not orphaned: no dialog\n%s", screen(m))
	}
	if got := footerText(m)[0]; got != "run 14 is still running (pid 51230): stop it with s" {
		t.Fatalf("the toast says why: %q", got)
	}
	afterGrace(&now)
	send(t, m, keyPress("c"))
	if len(acts.recovers) != 0 {
		t.Fatalf("a run whose owner is alive is never recovered: %v", acts.recovers)
	}
}

func TestWatchRecoverShouldToastWhenRunAlreadyFinished(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	src := &fakeWatchSource{done: []RunView{finished(12, "#49", "approved: merged", "a")}}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("c"))
	if dialogText(screen(m), "delamain recover") != "" {
		t.Fatalf("a finished run has nothing to recover: no dialog\n%s", screen(m))
	}
	if got := footerText(m)[0]; got != "run 12 already finished" {
		t.Fatalf("the toast says why: %q", got)
	}
	afterGrace(&now)
	send(t, m, keyPress("enter"))
	if len(acts.recovers) != 0 {
		t.Fatalf("a finished run is never recovered: %v", acts.recovers)
	}
}

func TestWatchRecoverShouldShowErrorToastWhenRecoverFails(t *testing.T) {
	now := watchNow
	st := lineStyles(termenv.ANSI)
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 999999, false)}}
	acts := &fakeWatchActions{recoverErr: errors.New("run 14: the run is not orphaned: its owner is alive")}
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return now }, actions: acts, after: noTimers}
	m := tea.Model(newWatchModel(src, st, opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m = send(t, settle(t, m, m.Init()), keyPress("c"))
	if got := border(m.View(), "delamain recover"); got != sgrOf(st.warn) {
		t.Fatalf("the recover dialog is a warning, as the stop: SGR %q", got)
	}
	afterGrace(&now)
	m = send(t, m, keyPress("c"))
	if len(acts.recovers) != 1 {
		t.Fatalf("the confirmation asks for the recover: %v", acts.recovers)
	}
	if got := footerText(m)[0]; got != "run 14: the run is not orphaned: its owner is alive" {
		t.Fatalf("a failed recover is the toast, not the running one: %q", got)
	}
	if got := sgrBefore(m.(watchModel).footer()[0], "run 14: the run is not orphaned"); got != sgrOf(st.bad) {
		t.Fatalf("a failed recover is a bad toast: SGR %q", got)
	}
}

func TestWatchActionsShouldRecoverOnlyTheGivenOrphanedRun(t *testing.T) {
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
	acts := f.actions(t, ctx, &bytes.Buffer{})

	out, err := acts.Recover(rec.ID)
	if err != nil || !strings.HasPrefix(out, "run 1 recovered:\n") || !strings.Contains(out, "halted") {
		t.Fatalf("Recover prints the result as recover does: %q %v", out, err)
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
	if _, err := acts.Recover(rec.ID); err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Fatalf("a run already recovered is refused: %v", err)
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

	afterGrace(&now)
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

	m = send(t, m, keyPress("d"))
	afterGrace(&now)
	m = send(t, m, keyPress("enter"))
	if len(acts.removes) != 1 || acts.removes[0] != (removeCall{12, false}) || footerText(m)[0] != "removed run 12" {
		t.Fatalf("enter removes the run and says so: %v %q", acts.removes, footerText(m))
	}
	if strings.Contains(screen(m), "delamain rm") {
		t.Fatalf("the dialog closes:\n%s", screen(m))
	}

	acts.removeErr = []error{errors.New("worktree /wt/x is in use by run 2 (implement)")}
	m = send(t, m, keyPress("d"))
	afterGrace(&now)
	m = send(t, m, keyPress("d"))
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
	acts := f.actions(t, ctx, &bytes.Buffer{})

	if out, err := acts.Remove(rec.ID, false); err != nil || out != "removed run 1\n" {
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

	_, err := acts.Remove(busy.ID, true)
	if want := "run 2 is still implement: stop it first with 'delamain stop 2'"; err == nil || err.Error() != want {
		t.Fatalf("an unfinished run is refused, even forced, want %q: %v", want, err)
	}
	if _, err := f.st.GetRun(ctx, busy.ID); err != nil {
		t.Fatalf("a refused run keeps its record: %v", err)
	}
	if _, err := acts.Remove(999, false); err == nil || err.Error() != "run 999 not found" {
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
	acts := f.actions(t, ctx, &bytes.Buffer{})

	_, err := acts.Remove(rec.ID, false)
	if !errors.Is(err, errDirtyWorktree) {
		t.Fatalf("a dirty worktree is errDirtyWorktree: %v", err)
	}
	if want := "worktree " + rec.Worktree + " has uncommitted changes; use --force to discard them"; err.Error() != want {
		t.Fatalf("the message is rm's, want %q: %q", want, err)
	}
	if !exists(filepath.Join(rec.Worktree, "wip.go")) || !exists(dir) {
		t.Fatal("a refused run keeps its worktree and directory")
	}

	if out, err := acts.Remove(rec.ID, true); err != nil || out != "removed run 1\n" {
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
		"file:///etc/passwd", "http://github.com/o/r/pull/7",
		"https://user:pw@github.com/o/r/pull/7", //gitleaks:allow // test data: OpenURL refuses a URL carrying credentials
		"-https://github.com", "https:///o/r", "javascript:alert(1)", "/usr/share/applications/x.desktop", "",
	} {
		if err := acts.OpenURL(bad); err == nil {
			t.Fatalf("%q is refused", bad)
		}
	}
	// OpenURL waits for the opener's exit code, so an accepted URL has already run it by the
	// time it returns: the file is there, or no process was ever started.
	if exists(got) {
		t.Fatal("a refused URL starts no process")
	}

	url := "https://gitlab.example.com/g/r/-/merge_requests/7"
	if err := acts.OpenURL(url); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != url { //nolint:gosec // inside the test's temp dir
		t.Fatalf("the opener gets the URL as its only argument: %q %v", b, err)
	}

	acts.opener = filepath.Join(dir, "missing")
	if err := acts.OpenURL(url); err == nil {
		t.Fatal("an opener that cannot start is an error")
	}
}

// Without a handler for https, xdg-open exits with a code of its own and opens nothing: o would
// look like it did nothing at all.
func TestWatchActionsShouldWarnWhenTheOpenerExitsWithAnError(t *testing.T) {
	dir := t.TempDir()
	opener := filepath.Join(dir, "opener")
	if err := os.WriteFile(opener, []byte("#!/bin/sh\nexit 3\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	acts := &storeWatchActions{opener: opener}
	err := acts.OpenURL("https://github.com/o/r/pull/7")
	if err == nil {
		t.Fatal("an opener that exits with an error is reported, not dropped")
	}
	for _, want := range []string{"could not open https://github.com/o/r/pull/7", "exit status 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the warning says %q: %v", want, err)
		}
	}
}

func TestWatchCopyShouldCopyTheRunID(t *testing.T) {
	now := watchNow
	evil := owned(14, "#55\x1b]52;c;ZXZpbA==\x07", "title\x1b]52;c;ZXZpbA==\x07", 51230, true)
	src := &fakeWatchSource{active: []RunView{evil}}
	acts := &fakeWatchActions{}
	m := send(t, startActions(t, src, acts, 160, 40, &now), keyPress("y"))
	if len(acts.copied) != 1 || acts.copied[0] != "14" {
		t.Fatalf("y copies only the run ID: %q", acts.copied)
	}
	if got := footerText(m)[0]; got != "run 14 copied" {
		t.Fatalf("the copy is the toast: %q", got)
	}

	acts.copyErr = errors.New("write /dev/pts/3: input/output error")
	if m = send(t, m, keyPress("y")); footerText(m)[0] != "write /dev/pts/3: input/output error" {
		t.Fatalf("a failed copy is the toast: %q", footerText(m))
	}
}

func TestWatchActionsShouldCopyThroughOSC52AndWrapItForTmux(t *testing.T) {
	for _, c := range []struct{ tmux, want string }{
		{"", "\x1b]52;c;MTQ=\x07"}, // base64("14")
		{"/tmp/tmux-1000/default,4242,0", "\x1bPtmux;\x1b\x1b]52;c;MTQ=\x07\x1b\\"}, // tmux passes it on
	} {
		t.Setenv("TMUX", c.tmux)
		var out bytes.Buffer
		acts := newStoreWatchActions(context.Background(), config.Config{}, nil, &out)
		if err := acts.Copy("14"); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != c.want {
			t.Fatalf("TMUX=%q: wrote %q, want %q", c.tmux, got, c.want)
		}
	}
}

// helpRows are the rows the help must show: each key and what it does.
var helpRows = [][2]string{
	{"↑ ↓ · j k", "pick the run; the status and the logs follow it"},
	{"tab", "move the focus: the runs, the queue (when it scrolls), the logs"},
	{"↑ ↓ · g G", "scroll the queue when it has the focus"},
	{"pgup pgdn · g G", "scroll the logs; G follows the end again"},
	{"1 … 6", "filter the source"},
	{"f", "follow the end, or stop following it"},
	{"z", "the logs in full screen"},
	{"enter", "the full status (medium and compact)"},
	{"e", "expand the finished runs (compact)"},
	{"← →", "switch the tab (mini)"},
	{"s · d", "stop the run (delamain stop) · remove it (delamain rm)"},
	{"c", "recover a run whose owner died (delamain recover)"},
	{"o · y", "open the PR in the browser · copy the run ID"},
	{"q · ctrl+c", "quit; the runs go on in the daemon"},
}

func TestWatchHelpShouldListEveryKeyAndCloseWithEscOrQ(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	m := send(t, startActions(t, src, &fakeWatchActions{}, 160, 40, &now), keyPress("?"))
	text := dialogText(screen(m), "help")
	if !strings.Contains(text, "the list and the logs reload on their own every 1ms") {
		t.Fatalf("the help says how often it reloads:\n%s", screen(m))
	}
	for _, k := range helpRows {
		if !strings.Contains(text, k[0]+" "+k[1]) {
			t.Fatalf("the help lists %q:\n%s", k, screen(m))
		}
	}

	for _, close := range []string{"esc", "?", "q"} {
		m = send(t, m, keyPress("?"))
		next, cmd := m.Update(keyPress(close))
		if quits(cmd) {
			t.Fatalf("%s closes the help, never the TUI", close)
		}
		if m = settle(t, next, cmd); strings.Contains(screen(m), "╭ help") {
			t.Fatalf("%s closes the help:\n%s", close, screen(m))
		}
	}
	m = send(t, m, keyPress("?"))
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); !quits(cmd) {
		t.Fatal("ctrl+c still quits with the help open")
	}
}

func TestWatchHelpShouldListRecoverKey(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 999999, false)}}
	m := send(t, startActions(t, src, &fakeWatchActions{}, 160, 40, &now), keyPress("?"))
	text := dialogText(screen(m), "help")
	want := "c recover a run whose owner died (delamain recover)"
	if !strings.Contains(text, want) {
		t.Fatalf("the help lists %q:\n%s", want, screen(m))
	}
	if i, j := strings.Index(text, "s · d "), strings.Index(text, want); i < 0 || j < i {
		t.Fatalf("c comes after s · d:\n%s", text)
	}
	if long := newWatchCmd(nil).Long; !strings.Contains(words(long), "c recovers it as delamain recover does when its owner died") {
		t.Fatalf("the help of watch tells c:\n%s", long)
	}
}

func TestWatchHelpShouldScrollTheKeysWhenItDoesNotFitTheHeight(t *testing.T) {
	now := watchNow
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}}
	m := send(t, startActions(t, src, &fakeWatchActions{}, 44, 14, &now), keyPress("?"))
	fits(t, m.View(), 44, 14)
	first, last := helpRows[0], helpRows[len(helpRows)-1]
	text := dialogText(screen(m), "help")
	if !strings.Contains(text, "reload on their own") || !strings.Contains(text, first[0]) || strings.Contains(text, last[0]) {
		t.Fatalf("the help starts at the top of the keys:\n%s", screen(m))
	}

	seen := text
	for range 40 {
		m = send(t, m, keyPress("down"))
		fits(t, m.View(), 44, 14)
		seen += " " + dialogText(screen(m), "help")
	}
	text = dialogText(screen(m), "help")
	if !strings.Contains(text, "reload on their own") || strings.Contains(text, first[0]) || !strings.Contains(text, last[0]+" "+last[1]) {
		t.Fatalf("↓ scrolls the keys to the last one, under the fixed text:\n%s", screen(m))
	}
	for _, k := range helpRows {
		if !strings.Contains(seen, k[1]) {
			t.Fatalf("scrolling shows every key, %q is missing", k[1])
		}
	}
	for _, row := range panelRows(screen(m), "help") {
		if strings.HasSuffix(strings.TrimSpace(row), "…") {
			t.Fatalf("no text of the help is cut:\n%s", screen(m))
		}
	}
	for range 40 {
		m = send(t, m, keyPress("k"))
	}
	if text = dialogText(screen(m), "help"); !strings.Contains(text, first[0]) {
		t.Fatalf("↑ and k scroll back to the first key:\n%s", screen(m))
	}
}

// dialogCase opens a dialog on a fresh TUI and says what it must show.
type dialogCase struct {
	name, title string
	open        []string // keys, from the first run selected
	acts        *fakeWatchActions
	want        []string // phrases of the dialog, spaces collapsed
	buttons     string
}

func dialogCases() []dialogCase {
	dirty := fmt.Errorf("worktree /home/someone/projects/.worktrees/delamain/feat/49-logs-colors-by-source-level-gate %w", errDirtyWorktree)
	return []dialogCase{
		{"stop", "delamain stop", []string{"s"}, &fakeWatchActions{}, []string{
			"Stop run 14?", "#55 artifacts: accept test_file and format_check in .delamain.yaml",
			"pid 51230 (alive): only this run is cancelled", "halted: stopped by the user (delamain stop)",
			"draft of what was already committed + needs-human label",
		}, "[ stop s ] [ cancel esc ]"},
		{"stop dead owner", "delamain stop", []string{"j", "s"}, &fakeWatchActions{}, []string{
			"Stop run 15?", "pid 999999 (dead): the run is finalized here, as delamain recover does",
		}, "[ stop s ] [ cancel esc ]"},
		{"stop finished", "delamain stop", []string{"G", "s"}, &fakeWatchActions{}, []string{
			"run 12 already finished", "recorded", "approved: merged",
		}, "[ ok esc ]"},
		{"rm active", "delamain rm", []string{"d"}, &fakeWatchActions{}, []string{
			"run 14 is still implement", "stop it first with s: rm only deletes finished runs",
		}, "[ ok esc ]"},
		{"rm finished", "delamain rm", []string{"G", "d"}, &fakeWatchActions{}, []string{
			"Remove run 12?", "#49 logs: colors by source, level, gate and agent marker",
			"record, run directory and worktree", "branch feat/49-logs-colors-by-source-level-gate and the PR",
			"the day's spend does not change (token_ledger stays)",
		}, "[ remove d ] [ cancel esc ]"},
		{"rm dirty", "delamain rm", []string{"G", "d", "d"}, &fakeWatchActions{removeErr: []error{dirty}}, []string{
			"Remove run 12?", dirty.Error(),
		}, "[ remove anyway F ] [ cancel esc ]"},
		{"recover", "delamain recover", []string{"j", "c"}, &fakeWatchActions{}, []string{
			"Recover run 15?", "#56 watch: actions", "pid 999999 (dead)",
			"draft of what was already committed; empty work is removed", "halted: the run was interrupted",
		}, "[ recover c ] [ cancel esc ]"},
		{"help", "help", []string{"?"}, &fakeWatchActions{}, []string{
			"the list and the logs reload on their own every 1ms", "quit; the runs go on in the daemon",
		}, ""},
	}
}

// dialogRuns are an active run, one whose owner died and a finished one, with long texts.
func dialogRuns() *fakeWatchSource {
	done := finished(12, "#49", "approved: merged", "logs: colors by source, level, gate and agent marker")
	done.Branch = "feat/49-logs-colors-by-source-level-gate"
	return &fakeWatchSource{
		active: []RunView{
			owned(14, "#55", "artifacts: accept test_file and format_check in .delamain.yaml", 51230, true),
			owned(15, "#56", "watch: actions", 999999, false),
		},
		done: []RunView{done},
	}
}

func TestWatchDialogsShouldFitEveryLayout(t *testing.T) {
	for _, size := range [][2]int{{200, 50}, {84, 26}, {44, 14}} {
		w, h := size[0], size[1]
		for _, c := range dialogCases() {
			now := watchNow
			m := startActions(t, dialogRuns(), c.acts, w, h, &now)
			for _, k := range c.open {
				m = send(t, m, keyPress(k))
				afterGrace(&now) // every dialog here is read before it is answered
			}
			if label := map[bool]string{true: "owner    pid", false: "owner      pid"}[w < 100]; c.name == "stop" && !strings.Contains(panelText(screen(m), c.title), label) {
				t.Fatalf("%dx%d: the labels take %d columns:\n%s", w, h, len(label)-3, screen(m))
			}
			var seen []string
			for i := range 30 {
				if i > 0 {
					m = send(t, m, keyPress("down"))
				}
				view := screen(m)
				fits(t, m.View(), w, h)
				rows := panelRows(view, c.title)
				if rows == nil {
					t.Fatalf("%dx%d %s: no dialog:\n%s", w, h, c.name, view)
				}
				if bw := lipgloss.Width(rows[0]) + 4; bw != min(64, w-4) {
					t.Fatalf("%dx%d %s: the dialog is %d wide, want min(64, %d-4)", w, h, c.name, bw, w)
				}
				for _, r := range rows {
					if strings.HasSuffix(strings.TrimSpace(r), "…") {
						t.Fatalf("%dx%d %s: a line is cut:\n%s", w, h, c.name, view)
					}
				}
				text := dialogText(view, c.title)
				if c.buttons != "" && !strings.Contains(text, c.buttons) {
					t.Fatalf("%dx%d %s: the buttons are always shown:\n%s", w, h, c.name, view)
				}
				seen = append(seen, nospace(text)) // a word longer than a line is broken in two
			}
			for _, want := range c.want {
				if !slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, nospace(want)) }) {
					t.Fatalf("%dx%d %s: %q is never shown whole:\n%s", w, h, c.name, want, strings.Join(seen, "\n"))
				}
			}
		}
	}
}

func TestWatchDialogsShouldStripControlCharactersFromRunData(t *testing.T) {
	evil := "evil\x1b]52;c;ZXZpbA==\x07\x1b[2J\u009b6n\x00"
	v := owned(14, "#55"+evil, "title "+evil, 51230, true)
	orphan := owned(15, "#56"+evil, "title "+evil, 999999, false)
	done := finished(12, "#49"+evil, "approved: "+evil, "title "+evil)
	done.Branch = "feat/" + evil
	src := &fakeWatchSource{active: []RunView{v, orphan}, done: []RunView{done}}
	acts := &fakeWatchActions{
		stopErr:    errors.New("stop " + evil),
		removeErr:  []error{fmt.Errorf("worktree /wt/%s %w", evil, errDirtyWorktree)},
		recoverErr: errors.New("recover " + evil),
	}
	now := watchNow
	m := startActions(t, src, acts, 160, 40, &now)
	views := map[string]string{
		"stop":          send(t, m, keyPress("s")).View(),
		"stop failed":   send(t, send(t, m, keyPress("s")), keyPress("s")).View(),
		"stop finished": send(t, send(t, m, keyPress("G")), keyPress("s")).View(),
		"rm":            send(t, send(t, m, keyPress("G")), keyPress("d")).View(),
		"rm dirty":      send(t, send(t, send(t, m, keyPress("G")), keyPress("d")), keyPress("d")).View(),
		"rm active":     send(t, m, keyPress("d")).View(),
	}
	acts.removeErr = []error{errors.New("rm " + evil)}
	views["rm other error"] = send(t, send(t, send(t, m, keyPress("G")), keyPress("d")), keyPress("d")).View()
	recovering := send(t, send(t, m, keyPress("j")), keyPress("c"))
	views["recover"] = recovering.View()
	afterGrace(&now)
	failed := send(t, recovering, keyPress("c"))
	if len(acts.recovers) != 1 || !strings.Contains(footerText(failed)[0], "recover evil") {
		t.Fatalf("the recover failed with the error of the run: %v %q", acts.recovers, footerText(failed))
	}
	views["recover failed"] = failed.View()
	for name, view := range views {
		if i := strings.IndexFunc(view, func(r rune) bool { return r != '\n' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) }); i >= 0 {
			t.Fatalf("%s: control character %q reached the terminal:\n%q", name, view[i], view)
		}
		if !strings.Contains(nospace(view), "evil]52;c;ZXZpbA==[2J6n") {
			t.Fatalf("%s: only the control characters go, the text stays:\n%s", name, view)
		}
	}
}

func TestWatchLongShouldEndWithTheTmuxPopupAndOSC52(t *testing.T) {
	long := newWatchCmd(nil).Long
	want := "In tmux, open it in a floating popup:\n\n" +
		"  bind D display-popup -E -w 50% -h 60% \"delamain watch\"\n\n" +
		"y copies the run ID through OSC 52; tmux needs `set -g set-clipboard on`."
	if !strings.HasSuffix(long, want) {
		t.Fatalf("the help of watch ends with the tmux section, want %q:\n%s", want, long)
	}
	if strings.Contains(long, "read-only") {
		t.Fatalf("watch is no longer read-only:\n%s", long)
	}
}

// d opens the rm dialog and d confirms it; s does the same for the stop. A double tap, or the
// autorepeat of a held key, would answer before the dialog could be read: rm deletes the run's
// directory, and stop throws away what a run that is going had done.
func TestWatchDialogsShouldIgnoreTheKeyThatOpenedThemUntilTheyCanBeRead(t *testing.T) {
	for _, c := range []struct {
		name, key, title, toast string
		src                     *fakeWatchSource
		acts                    *fakeWatchActions
		acted                   func(*fakeWatchActions) int
	}{
		{
			name: "rm", key: "d", title: "delamain rm", toast: "removed run 12",
			src:   &fakeWatchSource{done: []RunView{removable(12)}},
			acts:  &fakeWatchActions{removeOut: "removed run 12\n"},
			acted: func(f *fakeWatchActions) int { return len(f.removes) },
		},
		{
			name: "stop", key: "s", title: "delamain stop", toast: "stop requested for run 14 (owner pid 51230)",
			src:   &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}},
			acts:  &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n"},
			acted: func(f *fakeWatchActions) int { return len(f.stops) },
		},
		{
			name: "recover", key: "c", title: "delamain recover", toast: "run 15 recovered:",
			src:   &fakeWatchSource{active: []RunView{owned(15, "#56", "a", 999999, false)}},
			acts:  &fakeWatchActions{recoverOut: "run 15 recovered:\n"},
			acted: func(f *fakeWatchActions) int { return len(f.recovers) },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := watchNow
			m := startActions(t, c.src, c.acts, 160, 40, &now)

			m = send(t, m, keyPress(c.key))
			for _, k := range []string{c.key, "enter"} {
				m = send(t, m, keyPress(k))
				if c.acted(c.acts) != 0 {
					t.Fatalf("%s right after the dialog opened is a double tap, not a choice", k)
				}
				if dialogText(screen(m), c.title) == "" {
					t.Fatalf("%s leaves the dialog open to be read:\n%s", k, screen(m))
				}
			}

			now = now.Add(confirmGrace - time.Millisecond)
			if m = send(t, m, keyPress(c.key)); c.acted(c.acts) != 0 {
				t.Fatalf("the grace is %s, not less", confirmGrace)
			}
			afterGrace(&now)
			m = send(t, m, keyPress(c.key))
			if n := c.acted(c.acts); n != 1 {
				t.Fatalf("%s acts once the dialog could be read, %d times instead", c.key, n)
			}
			if got := footerText(m)[0]; got != c.toast {
				t.Fatalf("what went through is the toast, want %q: %q", c.toast, got)
			}
			if dialogText(screen(m), c.title) != "" {
				t.Fatalf("the answered dialog closes:\n%s", screen(m))
			}
		})
	}
}

func TestWatchActionsShouldDoNothingWithNoRunSelectedOrWhileOneRuns(t *testing.T) {
	now := watchNow
	acts := &fakeWatchActions{}
	m := startActions(t, &fakeWatchSource{}, acts, 160, 40, &now)
	for _, k := range []string{"s", "d", "c", "o", "y"} {
		next, cmd := m.Update(keyPress(k))
		if cmd != nil {
			t.Fatalf("%s acts on no run when the list is empty", k)
		}
		if got := footerText(next)[0]; strings.HasPrefix(got, "run ") {
			t.Fatalf("%s has no run to toast about: %q", k, got)
		}
		for _, open := range []string{"delamain stop", "delamain rm", "delamain recover"} {
			if dialogText(screen(next), open) != "" {
				t.Fatalf("%s opens no dialog without a run:\n%s", k, screen(next))
			}
		}
		m = settle(t, next, cmd)
	}
	if len(acts.stops) != 0 || len(acts.removes) != 0 || len(acts.recovers) != 0 || len(acts.opened) != 0 || len(acts.copied) != 0 {
		t.Fatalf("an empty list has nothing to act on: %+v", acts)
	}

	// A remove that runs keeps the dialog open: a second d must wait for it, as the store takes
	// one writer at a time.
	src := &fakeWatchSource{done: []RunView{removable(21)}}
	m = startActions(t, src, acts, 160, 40, &now)
	m = send(t, m, keyPress("d"))
	afterGrace(&now)
	next, _ := m.Update(keyPress("d")) // the Cmd is not run: the remove is still going
	if _, cmd := next.Update(keyPress("d")); cmd != nil {
		t.Fatal("a remove waits for the running one to end")
	}
	if _, cmd := next.Update(keyPress("F")); cmd != nil {
		t.Fatal("F waits for the running remove too")
	}

	// A recover waits for the running stop: both write the same store.
	src = &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true), owned(15, "#56", "b", 999999, false)}}
	m = startActions(t, src, acts, 160, 40, &now)
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	next, _ = m.Update(keyPress("s")) // the Cmd is not run: the stop is still going
	next = send(t, send(t, next, keyPress("j")), keyPress("c"))
	afterGrace(&now)
	if _, cmd := next.Update(keyPress("c")); cmd != nil {
		t.Fatal("a recover waits for the running stop to end")
	}
}

func TestWatchToastShouldStayThroughReloadsAndComeBackAfterAQuickOne(t *testing.T) {
	now := watchNow
	timers := &fakeTimers{now: &now}
	withPR := owned(14, "#55", "a", 51230, true)
	withPR.PR = "https://github.com/o/r/pull/7"
	src := &fakeWatchSource{active: []RunView{withPR}}
	acts := &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n"}
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return now }, actions: acts, after: timers.after}
	m := tea.Model(newWatchModel(src, noColors(), opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m = settle(t, m, m.Init())

	// A toast is on its own 5s: the reloads in between neither drop it nor cut it short.
	m = send(t, send(t, m, keyPress("y")), keyPress("y"))
	if got := footerText(m)[0]; got != "run 14 copied" {
		t.Fatalf("y says what it copied: %q", got)
	}
	m = timers.elapse(t, reload(t, m), 2*time.Second)
	m = reload(t, m)
	if got := footerText(m)[0]; got != "run 14 copied" {
		t.Fatalf("a reload is not what ends a toast: %q", got)
	}
	if m = timers.elapse(t, m, 3*time.Second); len(footerText(m)) != 1 {
		t.Fatalf("the toast still goes after its 5s: %q", footerText(m))
	}

	// A copy beside a running stop takes the footer for its 5s; the stop is still running after
	// it, so its toast comes back instead of leaving the footer silent.
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	m, cmd := m.Update(keyPress("s"))
	if got := footerText(m)[0]; got != "stopping run 14…" {
		t.Fatalf("the stop that runs is on the footer: %q", got)
	}
	m = send(t, m, keyPress("y"))
	if got := footerText(m)[0]; got != "run 14 copied" {
		t.Fatalf("the copy says so even with a stop running: %q", got)
	}
	if m = timers.elapse(t, m, 5*time.Second); footerText(m)[0] != "stopping run 14…" {
		t.Fatalf("the toast of the running stop comes back: %q", footerText(m))
	}
	m = settle(t, m, cmd)
	if got := footerText(m)[0]; got != "stop requested for run 14 (owner pid 51230)" {
		t.Fatalf("what the stop printed replaces it when it ends: %q", got)
	}
	if m = timers.elapse(t, m, 5*time.Second); len(footerText(m)) != 1 {
		t.Fatalf("the stop ended: nothing comes back after this toast: %q", footerText(m))
	}
}

// sgrOf is the SGR parameters look paints with.
func sgrOf(look lipgloss.Style) string { return sgrBefore(paint(look, "x"), "x") }

func TestWatchDialogsAndToastsShouldCarryTheColorOfWhatTheyDo(t *testing.T) {
	now := watchNow
	st := lineStyles(termenv.ANSI)
	src := &fakeWatchSource{active: []RunView{owned(14, "#55", "a", 51230, true)}, done: []RunView{removable(12)}}
	acts := &fakeWatchActions{stopOut: "stop requested for run 14 (owner pid 51230)\n", removeOut: "removed run 12\n"}
	opts := watchOptions{limit: 20, interval: time.Millisecond, now: func() time.Time { return now }, actions: acts, after: noTimers}
	m := tea.Model(newWatchModel(src, st, opts))
	m = send(t, m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m = settle(t, m, m.Init())

	look := func(name, title string, want lipgloss.Style) {
		t.Helper()
		if got := border(m.View(), title); got != sgrOf(want) {
			t.Fatalf("%s: the border of %q is SGR %q, want %q", name, title, got, sgrOf(want))
		}
	}
	toast := func(name, text string, want lipgloss.Style) {
		t.Helper()
		if got := sgrBefore(m.(watchModel).footer()[0], text); got != sgrOf(want) {
			t.Fatalf("%s: the toast %q is SGR %q, want %q", name, text, got, sgrOf(want))
		}
	}

	m = send(t, m, keyPress("?"))
	look("help", "help", st.info)
	m = send(t, send(t, m, keyPress("esc")), keyPress("s"))
	look("stop", "delamain stop", st.warn)

	afterGrace(&now)
	next, cmd := m.Update(keyPress("s"))
	m = next
	toast("the stop that runs", "stopping run 14…", st.warn)
	m = settle(t, m, cmd)
	toast("what the stop printed", "stop requested for run 14 (owner pid 51230)", st.ok)

	acts.stopErr = errors.New("run 14: database is locked")
	m = send(t, m, keyPress("s"))
	afterGrace(&now)
	m = send(t, m, keyPress("s"))
	toast("a failed stop", "run 14: database is locked", st.bad)

	m = send(t, send(t, m, keyPress("j")), keyPress("d"))
	look("rm", "delamain rm", st.bad)
	afterGrace(&now)
	m = send(t, m, keyPress("d"))
	toast("what rm printed", "removed run 12", st.ok)
}

func TestWatchActionsShouldRunUnderTheContextOfTheCommand(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	rec := f.run(t, "#1", "", store.StateImplement)

	// The TUI has no context to pass: without the command's, an action would outlive it and
	// quitting would wait on its mutex, with the alternate screen already closed.
	ended := f.actions(t, ctx, &bytes.Buffer{})
	cancel()
	if _, err := ended.Stop(rec.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("a stop ends with the command, got %v", err)
	}
	if _, err := ended.Remove(rec.ID, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("a remove ends with the command, got %v", err)
	}

	closed := f.actions(t, context.Background(), &bytes.Buffer{})
	closed.close()
	if _, err := closed.Stop(rec.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("quitting ends a running action instead of waiting for it, got %v", err)
	}
}

// The TUI holds the terminal: a recover of an orphan writes its warnings through the runner's
// logger, which would be drawn over the screen if it were the default stderr one.
func TestWatchActionsShouldKeepTheRecoverLogsOffTheTerminal(t *testing.T) {
	f := newFixture(t)
	acts := f.actions(t, context.Background(), &bytes.Buffer{})
	if acts.stop.log == nil {
		t.Fatal("the stopper of watch carries a logger of its own, never the default stderr one")
	}

	var logs bytes.Buffer
	cfg, err := config.Load(f.cfgFile, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRunner(cfg, f.st, notify.Options{Log: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Notify.Close() })
	r.Log.Warn("recovered", "run", 1)
	if !strings.Contains(logs.String(), "recovered") {
		t.Fatalf("the runner logs where its caller asked: %q", logs.String())
	}
}

// launchScript is an executable at dir/delamain that runs body, as the delamain StartIssue runs.
func launchScript(t *testing.T, dir, body string) func() (string, error) {
	t.Helper()
	exe := filepath.Join(dir, "delamain")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return func() (string, error) { return exe, nil }
}

// launchActions are the actions of watch on the state dir state, running exe.
func launchActions(state string, exe func() (string, error)) *storeWatchActions {
	cfg := config.Config{}
	cfg.Paths.StateDir = state
	acts := newStoreWatchActions(context.Background(), cfg, nil, &bytes.Buffer{})
	acts.executable = exe
	return acts
}

// launchLogs are the logs StartIssue wrote under state.
func launchLogs(t *testing.T, state string) []string {
	t.Helper()
	logs, err := filepath.Glob(filepath.Join(state, "launch", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestStartIssueShouldLaunchDetachedRunWithArgsList(t *testing.T) {
	cmd := launchCmd("/bin/delamain", "/src/a b; rm -rf ~", 42, os.Stderr)
	if cmd.Path != "/bin/delamain" || !slices.Equal(cmd.Args, []string{"/bin/delamain", "run", "--issue", "42", "--repo", "/src/a b; rm -rf ~"}) {
		t.Fatalf("the run is the binary with a list of arguments, no shell: %q %q", cmd.Path, cmd.Args)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("the run starts a session of its own: the terminal of the watch does not end it")
	}
	if cmd.Stdin != nil || cmd.Stdout != os.Stderr || cmd.Stderr != os.Stderr {
		t.Fatalf("the run reads nothing and writes to the log alone: %v %v %v", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}

	dir := t.TempDir()
	state, args := filepath.Join(dir, "state"), filepath.Join(dir, "args")
	repo := filepath.Join(dir, "a b; touch pwned")
	exe := launchScript(t, dir, "printf '%s\\n' \"$@\" > '"+args+"'\necho to stdout\necho to stderr >&2\nread line || echo stdin is empty\n")
	if err := launchActions(state, exe).StartIssue(repo, 42); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(args); err != nil || string(b) != "run\n--issue\n42\n--repo\n"+repo+"\n" { //nolint:gosec // inside the test's temp dir
		t.Fatalf("the run gets the issue and the repo as arguments: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("the repo never goes through a shell")
	}
	logs := launchLogs(t, state)
	if len(logs) != 1 || !strings.HasSuffix(logs[0], "-issue-42.log") {
		t.Fatalf("the run writes one log in launch/, named after the issue: %v", logs)
	}
	if b, err := os.ReadFile(logs[0]); err != nil || string(b) != "to stdout\nto stderr\nstdin is empty\n" {
		t.Fatalf("stdout and stderr go to the log, stdin is empty: %q %v", b, err)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(state, "launch"): 0o700, logs[0]: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s is %v, want %v: %v", path, fi.Mode().Perm(), want, err)
		}
	}
}

func TestStartIssueShouldKeepTheRunAliveWhenTheWatchCloses(t *testing.T) {
	dir := t.TempDir()
	state, release, done := filepath.Join(dir, "state"), filepath.Join(dir, "release"), filepath.Join(dir, "done")
	// The run waits for the test to release it, so it is still running when the watch closes.
	exe := launchScript(t, dir, "echo started\nwhile [ ! -e '"+release+"' ]; do sleep 0.05; done\ntouch '"+done+"'\n")
	acts := launchActions(state, exe)
	ended := make(chan error, 1)
	go func() { ended <- acts.StartIssue(dir, 7) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if logs := launchLogs(t, state); len(logs) == 1 {
			if b, _ := os.ReadFile(logs[0]); string(b) == "started\n" { //nolint:gosec // inside the test's temp dir
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	closed := make(chan struct{})
	go func() { acts.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the watch does not wait for the run")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ended:
		if err != nil {
			t.Fatalf("the run went on to the end after the watch closed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended")
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatalf("the run was not killed with the watch: %v", err)
	}
}

func TestStartIssueShouldReportTheExitCodeAndTheLogOfAFailedRun(t *testing.T) {
	for _, c := range []struct {
		code int
		want string
	}{
		{5, "issue #7 not started: maximum parallelism reached; see "},
		{4, "issue #7 needs more information (exit 4); see "},
		{2, "issue #7 failed: exit 2; see "},
	} {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		err := launchActions(state, launchScript(t, dir, "exit "+strconv.Itoa(c.code)+"\n")).StartIssue(dir, 7)
		exit, ok := errors.AsType[startIssueExit](err)
		if !ok {
			t.Fatalf("exit %d is a startIssueExit: %v", c.code, err)
		}
		logs := launchLogs(t, state)
		if exit.code != c.code || len(logs) != 1 || exit.log != logs[0] {
			t.Fatalf("exit %d carries its code and the log: %+v %v", c.code, exit, logs)
		}
		if got := err.Error(); got != c.want+logs[0] {
			t.Errorf("exit %d says %q, want %q", c.code, got, c.want+logs[0])
		}
	}

	dir := t.TempDir()
	missing := func() (string, error) { return filepath.Join(dir, "missing"), nil }
	err := launchActions(filepath.Join(dir, "state"), missing).StartIssue(dir, 7)
	if _, ok := errors.AsType[startIssueExit](err); err == nil || ok {
		t.Fatalf("a run that cannot start is an error of its own: %v", err)
	}
}
