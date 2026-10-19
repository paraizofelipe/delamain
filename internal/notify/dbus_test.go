package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

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
