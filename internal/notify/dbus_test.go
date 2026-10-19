package notify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/paraizo/delamain/internal/config"
)

// fakeBus is a session bus whose notification server records every Notify call.
type fakeBus struct {
	mu       sync.Mutex
	sent     []notification
	nextID   uint32
	err      error
	closed   bool
	noOwner  bool                        // org.freedesktop.Notifications has no owner
	onNotify func(context.Context) error // inspects the call's context
	caps     []string
	server   [2]string   // name, version
	signals  chan signal // nil until subscribed
}

func (b *fakeBus) ServerInfo(context.Context) (string, string, error) {
	return b.server[0], b.server[1], nil
}

func (b *fakeBus) Capabilities(context.Context) ([]string, error) { return b.caps, nil }

func (b *fakeBus) Subscribe() (<-chan signal, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.signals = make(chan signal)
	return b.signals, nil
}

func (b *fakeBus) subscribed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.signals != nil
}

// emit delivers a signal and returns once the notifier took it.
func (b *fakeBus) emit(t *testing.T, s signal) {
	t.Helper()
	b.mu.Lock()
	ch := b.signals
	b.mu.Unlock()
	select {
	case ch <- s:
	case <-time.After(2 * time.Second):
		t.Fatalf("nobody listens for %s signals", s.name)
	}
}

func (b *fakeBus) NameHasOwner(name string) (bool, error) {
	return name == "org.freedesktop.Notifications" && !b.noOwner, nil
}

func (b *fakeBus) Notify(ctx context.Context, n notification) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.onNotify != nil {
		if err := b.onNotify(ctx); err != nil {
			return 0, err
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	b.sent = append(b.sent, n)
	if n.ReplacesID != 0 {
		return n.ReplacesID, nil
	}
	b.nextID++
	return b.nextID + 6, nil
}

func (b *fakeBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.signals != nil && !b.closed {
		close(b.signals) // as a closed connection does
	}
	b.closed = true
	return nil
}

func (b *fakeBus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *fakeBus) calls() []notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]notification(nil), b.sent...)
}

// syncBuffer is a log sink safe to read while a signal goroutine writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lastCall is the latest toast sent, failing the test when none was.
func (b *fakeBus) lastCall(t *testing.T) notification {
	t.Helper()
	calls := b.calls()
	if len(calls) == 0 {
		t.Fatal("no toast was sent")
	}
	return calls[len(calls)-1]
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// dbusSys is a Linux desktop whose session bus is b.
func dbusSys(b *fakeBus) sys {
	return sys{
		goos:   "linux",
		getenv: env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}),
		dial:   func(string) (bus, error) { return b, nil },
	}
}

func allEvents() []string {
	return []string{"started", "pr_opened", "needs_human", "halted", "needs_info", "budget", "recover"}
}

func newDBusClient(t *testing.T, b *fakeBus, opts Options) *Client {
	t.Helper()
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	if opts.Log == nil {
		opts.Log = discardLog()
	}
	c := newWith(config.Notify{Enabled: true, Backend: "dbus", Events: allEvents()}, opts, dbusSys(b))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDBusShouldSendAppIconImagePathUrgencyAndCategory(t *testing.T) {
	b := &fakeBus{}
	state := t.TempDir()
	c := newDBusClient(t, b, Options{StateDir: state})
	m := Message{Event: EventHalted, RunID: 42, Title: "⛔ Halted · delamain #12", Body: "line 1\nline 2",
		Urgency: Critical, Expire: 0, Category: "x-delamain.halted"}
	if err := c.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	calls := b.calls()
	if len(calls) != 1 {
		t.Fatalf("one toast expected, got %d", len(calls))
	}
	n, logo := calls[0], filepath.Join(state, "notify", "delamain.png")
	if n.AppName != "delamain" || n.AppIcon != logo || n.Summary != m.Title || n.Body != m.Body || n.Expire != 0 {
		t.Fatalf("notification = %+v, want app delamain, icon %s and the message text", n, logo)
	}
	want := map[string]dbus.Variant{
		"urgency":    dbus.MakeVariant(byte(2)),
		"category":   dbus.MakeVariant("x-delamain.halted"),
		"image-path": dbus.MakeVariant(logo),
	}
	for k, v := range want {
		if got, ok := n.Hints[k]; !ok || got.Signature() != v.Signature() || got.Value() != v.Value() {
			t.Errorf("hint %s = %v, want %v", k, got, v)
		}
	}
}

func TestDBusShouldEscapeMarkupCharacters(t *testing.T) {
	b := &fakeBus{}
	c := newDBusClient(t, b, Options{})
	m := Message{Event: EventPROpened, RunID: 1, Title: "a & <b>", Body: "x > y && <i>z</i>", Urgency: Normal, Expire: 15000}
	if err := c.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	n := b.lastCall(t)
	if n.Summary != "a &amp; &lt;b&gt;" || n.Body != "x &gt; y &amp;&amp; &lt;i&gt;z&lt;/i&gt;" {
		t.Fatalf("markup must be escaped: summary %q body %q", n.Summary, n.Body)
	}
}

func TestDBusShouldReplaceTheToastOfTheSameRun(t *testing.T) {
	b := &fakeBus{}
	c := newDBusClient(t, b, Options{})
	ctx := context.Background()
	for _, id := range []int64{42, 42, 43} {
		if err := c.Notify(ctx, Message{Event: EventStarted, RunID: id, Title: "t", Urgency: Low, Expire: 15000}); err != nil {
			t.Fatal(err)
		}
	}
	calls := b.calls()
	if len(calls) != 3 {
		t.Fatalf("three toasts expected, got %d", len(calls))
	}
	if calls[0].ReplacesID != 0 || calls[1].ReplacesID != 7 || calls[2].ReplacesID != 0 {
		t.Fatalf("run 42 must replace its toast 7 and run 43 open a new one: replaces=%d,%d,%d",
			calls[0].ReplacesID, calls[1].ReplacesID, calls[2].ReplacesID)
	}
}

// clickBus is a notification server that supports actions and emits signals.
func clickBus() *fakeBus { return &fakeBus{caps: []string{"body", "actions"}} }

// opener records the targets a click opens.
type opener chan string

func (o opener) open(_ context.Context, target string) error {
	o <- target
	return nil
}

// expect fails unless the next opened target is want.
func (o opener) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-o:
		if got != want {
			t.Fatalf("opened %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("nothing was opened, want %q", want)
	}
}

// fakeClock is a settable clock shared with the signal goroutine.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type daemonEnv struct {
	c     *Client
	b     *fakeBus
	open  opener
	clock *fakeClock
	log   *syncBuffer
	state string
}

// newDaemonClient is the daemon's notifier: interactive, allowed to open github.com URLs.
func newDaemonClient(t *testing.T, b *fakeBus) daemonEnv {
	t.Helper()
	e := daemonEnv{b: b, open: make(opener, 8), clock: &fakeClock{t: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)},
		log: &syncBuffer{}, state: t.TempDir()}
	s := dbusSys(b)
	s.open, s.now = e.open.open, e.clock.now
	e.c = newWith(config.Notify{Enabled: true, Backend: "dbus", Events: allEvents()},
		Options{Interactive: true, StateDir: e.state, Log: slogTo(e.log), ForgeHosts: []string{"github.com"}}, s)
	t.Cleanup(func() { _ = e.c.Close() })
	return e
}

// toast sends m and returns the notification id the server gave it.
func (e daemonEnv) toast(t *testing.T, m Message) uint32 {
	t.Helper()
	if err := e.c.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	e.b.mu.Lock()
	defer e.b.mu.Unlock()
	return e.b.nextID + 6
}

func withActions(runID int64, actions ...Action) Message {
	return Message{Event: EventPROpened, RunID: runID, Title: "t", Urgency: Normal, Expire: 15000, Actions: actions}
}

func TestDBusShouldOmitActionsWhenNotInteractive(t *testing.T) {
	b := clickBus()
	c := newDBusClient(t, b, Options{ForgeHosts: []string{"github.com"}})
	if err := c.Notify(context.Background(), FormatPROpened(sampleRun())); err != nil {
		t.Fatal(err)
	}
	if got := b.lastCall(t).Actions; len(got) != 0 {
		t.Fatalf("outside the daemon a toast has no actions, got %q", got)
	}

	e := newDaemonClient(t, clickBus())
	e.toast(t, FormatPROpened(sampleRun()))
	want := []string{"default", "Open PR", "open-pr", "Open PR", "open-issue", "Open issue"}
	if got := e.b.lastCall(t).Actions; !slices.Equal(got, want) {
		t.Fatalf("in the daemon the actions go flat as in the spec: %q, want %q", got, want)
	}
}

func TestDBusShouldOmitActionsWhenServerLacksActionsCapability(t *testing.T) {
	e := newDaemonClient(t, &fakeBus{caps: []string{"body", "body-markup"}})
	e.toast(t, FormatPROpened(sampleRun()))
	if got := e.b.lastCall(t).Actions; len(got) != 0 {
		t.Fatalf("a server without the actions capability gets none: %q", got)
	}
	if e.b.subscribed() {
		t.Fatal("without actions there is no click to listen for")
	}
}

func TestActionInvokedShouldOpenValidatedHTTPSURL(t *testing.T) {
	e := newDaemonClient(t, clickBus())
	id := e.toast(t, FormatPROpened(sampleRun()))
	e.b.emit(t, signal{name: actionInvoked, id: id, key: "open-pr"})
	e.open.expect(t, samplePR)
	e.b.emit(t, signal{name: actionInvoked, id: id, key: "default"})
	e.open.expect(t, samplePR)
	e.b.emit(t, signal{name: actionInvoked, id: id, key: "open-issue"})
	e.open.expect(t, sampleIssue)
}

func TestActionInvokedShouldRejectNonHTTPSOrForeignHostURL(t *testing.T) {
	e := newDaemonClient(t, clickBus())
	bad := []string{
		"http://github.com/acme/api/pull/1",
		"https://evil.example/acme/api/pull/1",
		"https://github.com.evil.example/acme/api/pull/1",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"github.com/acme/api/pull/1",
	}
	var actions []Action
	for i, target := range bad {
		actions = append(actions, Action{Key: fmt.Sprintf("bad-%d", i), Label: "x", Target: target})
	}
	id := e.toast(t, withActions(1, append(actions, Action{Key: "open-pr", Label: "Open PR", Target: samplePR})...))
	for _, a := range actions {
		e.b.emit(t, signal{name: actionInvoked, id: id, key: a.Key})
	}
	e.b.emit(t, signal{name: actionInvoked, id: id, key: "open-pr"})
	e.open.expect(t, samplePR) // the rejected targets, invoked first, opened nothing
	if n := strings.Count(e.log.String(), "level=WARN"); n != len(bad) {
		t.Fatalf("each rejected target is a warning, got %d:\n%s", n, e.log.String())
	}
}

func TestActionInvokedShouldRejectReportPathOutsideStateDir(t *testing.T) {
	e := newDaemonClient(t, clickBus())
	report := filepath.Join(e.state, "runs", "api", "7", "report.md")
	outside := filepath.Join(t.TempDir(), "report.md")
	link := filepath.Join(e.state, "runs", "api", "7", "link.md")
	for _, p := range []string{report, outside} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("# report"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		outside,
		filepath.Join(e.state, "runs", "..", "..", filepath.Base(filepath.Dir(outside)), "report.md"),
		link,
		filepath.Join("runs", "api", "7", "report.md"),
		filepath.Join(e.state, "runs", "api", "8", "report.md"), // does not exist
		e.state + "-other/report.md",
	}
	var actions []Action
	for i, target := range bad {
		actions = append(actions, Action{Key: fmt.Sprintf("bad-%d", i), Label: "x", Target: target})
	}
	id := e.toast(t, withActions(1, append(actions, Action{Key: "open-report", Label: "Open report", Target: report})...))
	for _, a := range actions {
		e.b.emit(t, signal{name: actionInvoked, id: id, key: a.Key})
	}
	e.b.emit(t, signal{name: actionInvoked, id: id, key: "open-report"})
	e.open.expect(t, report)
	if n := strings.Count(e.log.String(), "level=WARN"); n != len(bad) {
		t.Fatalf("each rejected path is a warning, got %d:\n%s", n, e.log.String())
	}
}

func TestNotificationClosedShouldForgetActions(t *testing.T) {
	e := newDaemonClient(t, clickBus())
	first := e.toast(t, withActions(1, Action{Key: "default", Label: "Open PR", Target: samplePR}))
	e.b.emit(t, signal{name: notificationClosed, id: first})
	e.b.emit(t, signal{name: actionInvoked, id: first, key: "default"})
	second := e.toast(t, withActions(2, Action{Key: "default", Label: "Open issue", Target: sampleIssue}))
	e.b.emit(t, signal{name: actionInvoked, id: second, key: "default"})
	e.open.expect(t, sampleIssue) // the closed toast's action opened nothing
}

func TestActionsShouldBeForgottenAfter24Hours(t *testing.T) {
	e := newDaemonClient(t, clickBus())
	old := e.toast(t, withActions(1, Action{Key: "default", Label: "Open PR", Target: samplePR}))
	e.clock.add(24*time.Hour + time.Second)
	e.b.emit(t, signal{name: actionInvoked, id: old, key: "default"})
	fresh := e.toast(t, withActions(2, Action{Key: "default", Label: "Open issue", Target: sampleIssue}))
	e.b.emit(t, signal{name: actionInvoked, id: fresh, key: "default"})
	e.open.expect(t, sampleIssue) // the day-old toast's action opened nothing

	e.clock.add(23 * time.Hour)
	e.b.emit(t, signal{name: actionInvoked, id: fresh, key: "default"})
	e.open.expect(t, sampleIssue) // under 24h it still opens
}
