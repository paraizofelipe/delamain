package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBus records what a notification server would receive.
type fakeBus struct {
	mu     sync.Mutex
	sent   []notification
	nextID uint32
	owned  bool  // a server owns org.freedesktop.Notifications
	err    error // returned by Notify
	// hang makes Notify ignore its context until the channel closes.
	hang     chan struct{}
	deadline time.Time // of the last Notify context
	caps     []string
	signals  chan busSignal
}

func (b *fakeBus) Capabilities(context.Context) ([]string, error) { return b.caps, nil }

func (b *fakeBus) Subscribe() (<-chan busSignal, error) { return b.signals, nil }

func (b *fakeBus) NameHasOwner(_ context.Context, name string) (bool, error) {
	return b.owned && name == "org.freedesktop.Notifications", nil
}

func (b *fakeBus) Notify(ctx context.Context, n notification) (uint32, error) {
	if b.hang != nil {
		<-b.hang
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deadline, _ = ctx.Deadline()
	if b.err != nil {
		return 0, b.err
	}
	b.sent = append(b.sent, n)
	b.nextID++
	return b.nextID, nil
}

func (b *fakeBus) Close() error { return nil }

func (b *fakeBus) last(t *testing.T) notification {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sent) == 0 {
		t.Fatal("nothing reached the notification server")
	}
	return b.sent[len(b.sent)-1]
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDBusShouldSendAppIconImagePathUrgencyAndCategory(t *testing.T) {
	state := t.TempDir()
	b := &fakeBus{}
	d := newDBus(b, Options{StateDir: state, Log: discard()})
	if err := d.Notify(context.Background(), FormatPROpened(example())); err != nil {
		t.Fatal(err)
	}

	icon := filepath.Join(state, "notify", "delamain.png")
	n := b.last(t)
	if n.AppName != "delamain" || n.AppIcon != icon || n.Expire != 15000 {
		t.Fatalf("app_name=%q app_icon=%q expire=%d", n.AppName, n.AppIcon, n.Expire)
	}
	if n.Summary != "✅ PR opened · delamain #12" || n.Body != "Mostrar HARNESS e INTERACTIONS no status\nPR #31 · 2 iterations · 167K tokens · US$ 0.67" {
		t.Fatalf("summary=%q body=%q", n.Summary, n.Body)
	}
	want := map[string]any{"urgency": byte(1), "category": "x-delamain.pr", "image-path": icon}
	if !reflect.DeepEqual(n.Hints, want) {
		t.Fatalf("hints = %#v, want %#v", n.Hints, want)
	}
}

func TestDBusShouldEscapeMarkupCharacters(t *testing.T) {
	b := &fakeBus{}
	d := newDBus(b, Options{StateDir: t.TempDir(), Log: discard()})
	m := Message{Title: "PR <b>&</b> · api", Body: "a < b && c > d\n<script>"}
	if err := d.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	n := b.last(t)
	if n.Summary != "PR &lt;b&gt;&amp;&lt;/b&gt; · api" || n.Body != "a &lt; b &amp;&amp; c &gt; d\n&lt;script&gt;" {
		t.Fatalf("servers that read markup must see plain text: summary=%q body=%q", n.Summary, n.Body)
	}
}

func TestDBusShouldReplaceTheToastOfTheSameRun(t *testing.T) {
	b := &fakeBus{nextID: 40}
	d := newDBus(b, Options{StateDir: t.TempDir(), Log: discard()})
	ctx := context.Background()
	send := func(m Message) uint32 {
		t.Helper()
		if err := d.Notify(ctx, m); err != nil {
			t.Fatal(err)
		}
		return b.last(t).ReplacesID
	}
	if id := send(Message{RunID: 7, Title: "started"}); id != 0 {
		t.Fatalf("a run's first toast is new, replaces_id=%d", id)
	}
	if id := send(Message{RunID: 9, Title: "other run"}); id != 0 {
		t.Fatalf("another run gets its own toast, replaces_id=%d", id)
	}
	if id := send(Message{RunID: 7, Title: "pr opened"}); id != 41 {
		t.Fatalf("the same run replaces its toast (id 41), replaces_id=%d", id)
	}
	if id := send(Message{RunID: 0, Title: "budget"}); id != 0 {
		t.Fatalf("toasts without a run never replace one, replaces_id=%d", id)
	}
	if id := send(Message{RunID: 0, Title: "recover"}); id != 0 {
		t.Fatalf("toasts without a run never replace one, replaces_id=%d", id)
	}
}

// clicks is a daemon's D-Bus backend on a server with actions, with xdg-open faked.
type clicks struct {
	d      *dbusBackend
	b      *fakeBus
	state  string
	opened chan string
	log    *bytes.Buffer
	now    time.Time
	mu     sync.Mutex
}

func newClicks(t *testing.T, caps ...string) *clicks {
	t.Helper()
	c := &clicks{
		b:     &fakeBus{caps: caps, signals: make(chan busSignal, 16)},
		state: t.TempDir(), opened: make(chan string, 8), log: &bytes.Buffer{},
		now: time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC),
	}
	c.d = newDBus(c.b, Options{
		Interactive: true, StateDir: c.state, Log: slog.New(slog.NewTextHandler(syncWriter{c}, nil)),
		open: func(_ context.Context, target string) error { c.opened <- target; return nil },
		now:  func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now },
	})
	t.Cleanup(func() { _ = c.d.Close() })
	return c
}

type syncWriter struct{ c *clicks }

func (w syncWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.log.Write(p)
}

func (c *clicks) logs() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.log.String()
}

func (c *clicks) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// send shows m and returns the id the server gave it.
func (c *clicks) send(t *testing.T, m Message) uint32 {
	t.Helper()
	if err := c.d.Notify(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return c.b.nextID
}

func (c *clicks) click(id uint32, key string) {
	c.b.signals <- busSignal{Name: "ActionInvoked", ID: id, Key: key}
}

// expectOpened waits for the next target xdg-open receives.
func (c *clicks) expectOpened(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-c.opened:
		if got != want {
			t.Fatalf("opened %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("nothing opened, want %q; log:\n%s", want, c.logs())
	}
}

// expectRejected clicks key on a toast with target, then proves a valid click still opens: the
// rejected one was handled first and opened nothing.
func (c *clicks) expectRejected(t *testing.T, target, forgeHost string) {
	t.Helper()
	id := c.send(t, Message{Title: "x", Actions: []Action{{Key: "default", Label: "Open", Target: target}}, ForgeHost: forgeHost})
	c.click(id, "default")
	ok := c.send(t, Message{Title: "y", Actions: []Action{{Key: "default", Label: "Open PR", Target: examplePR}}, ForgeHost: "github.com"})
	c.click(ok, "default")
	c.expectOpened(t, examplePR)
	if !strings.Contains(c.logs(), "level=WARN") {
		t.Fatalf("a rejected target is logged:\n%s", c.logs())
	}
}

func prToast() Message {
	m := FormatPROpened(example())
	m.ForgeHost = "github.com"
	return m
}

func TestDBusShouldOmitActionsWhenNotInteractive(t *testing.T) {
	b := &fakeBus{caps: []string{"actions", "body"}, signals: make(chan busSignal, 16)}
	d := newDBus(b, Options{StateDir: t.TempDir(), Log: discard()})
	defer d.Close()
	if err := d.Notify(context.Background(), prToast()); err != nil {
		t.Fatal(err)
	}
	if got := b.last(t).Actions; len(got) != 0 {
		t.Fatalf("`delamain run` has nobody to click, actions=%v", got)
	}
}

func TestDBusShouldOmitActionsWhenServerLacksActionsCapability(t *testing.T) {
	c := newClicks(t, "body", "body-markup")
	c.send(t, prToast())
	if got := c.b.last(t).Actions; len(got) != 0 {
		t.Fatalf("a server without actions gets none, actions=%v", got)
	}
}

func TestDBusShouldSendFlatActionListWhenInteractive(t *testing.T) {
	c := newClicks(t, "actions", "body")
	c.send(t, prToast())
	want := []string{"default", "Open PR", "open-pr", "Open PR", "open-issue", "Open issue"}
	if got := c.b.last(t).Actions; !slices.Equal(got, want) {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestActionInvokedShouldOpenValidatedHTTPSURL(t *testing.T) {
	c := newClicks(t, "actions")
	id := c.send(t, prToast())
	c.click(id, "default")
	c.expectOpened(t, examplePR)
	c.click(id, "open-issue")
	c.expectOpened(t, exampleIssue)

	report := filepath.Join(c.state, "runs", "delamain", "42", "report.md")
	if err := os.MkdirAll(filepath.Dir(report), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(report, []byte("# report"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := example()
	r.PRURL, r.Report, r.State = "", report, "plan"
	halted := FormatHalted(r)
	halted.ForgeHost = "github.com"
	c.click(c.send(t, halted), "default")
	c.expectOpened(t, report)
}

func TestActionInvokedShouldRejectNonHTTPSOrForeignHostURL(t *testing.T) {
	c := newClicks(t, "actions")
	for _, target := range []string{
		"http://github.com/paraizofelipe/delamain/pull/31",
		"https://evil.example/paraizofelipe/delamain/pull/31",
		"https://github.com.evil.example/pull/31",
		"https://github.com@evil.example/pull/31",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"--help",
	} {
		c.expectRejected(t, target, "github.com")
	}
	c.expectRejected(t, examplePR, "") // a toast without forge opens no URL
}

func TestActionInvokedShouldRejectReportPathOutsideStateDir(t *testing.T) {
	c := newClicks(t, "actions")
	outside := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.state, "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		outside,
		filepath.Join(c.state, "..", filepath.Base(filepath.Dir(outside)), "report.md"),
		link, // inside by name, outside once resolved
		filepath.Join(c.state, "runs", "gone", "report.md"), // never written
		"runs/delamain/42/report.md",                        // relative
		c.state,
	} {
		c.expectRejected(t, target, "github.com")
	}
}

func TestNotificationClosedShouldForgetActions(t *testing.T) {
	c := newClicks(t, "actions")
	id := c.send(t, prToast())
	c.b.signals <- busSignal{Name: "NotificationClosed", ID: id}
	c.click(id, "default")
	c.click(c.send(t, prToast()), "open-issue")
	c.expectOpened(t, exampleIssue) // the click on the closed toast opened nothing
}

func TestActionsShouldBeForgottenAfter24Hours(t *testing.T) {
	c := newClicks(t, "actions")
	id := c.send(t, prToast())
	c.advance(24*time.Hour + time.Second)
	c.click(id, "default")
	c.click(c.send(t, prToast()), "open-issue")
	c.expectOpened(t, exampleIssue)
}
