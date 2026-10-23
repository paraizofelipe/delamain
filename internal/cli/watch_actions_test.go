package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
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
