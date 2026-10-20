package notify

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
)

func autoClient(t *testing.T, s sys, log *syncBuffer) *Client {
	t.Helper()
	opts := Options{StateDir: t.TempDir(), Log: discardLog()}
	if log != nil {
		opts.Log = slogTo(log)
	}
	c := newWith(config.Notify{Enabled: true, Backend: "auto", Events: allEvents()}, opts, s)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAutoShouldPickMacOSOnDarwin(t *testing.T) {
	x := &fakeExec{}
	s := macSys(x, true)
	dialed := false
	s.getenv = env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/tmp/bus"})
	s.dial = func(context.Context, string) (bus, error) { dialed = true; return &fakeBus{}, nil }
	c := autoClient(t, s, nil)
	if c.Backend() != "macos" || dialed {
		t.Fatalf("darwin uses Notification Center even with a bus address: backend=%s dialed=%v", c.Backend(), dialed)
	}
	if err := c.Notify(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := x.last(t); got[0] != "/opt/homebrew/bin/terminal-notifier" {
		t.Fatalf("toast must go through terminal-notifier: %q", got)
	}
}

func TestAutoShouldPickDBusWhenNotificationServerOwnsName(t *testing.T) {
	b := &fakeBus{}
	var addr string
	s := dbusSys(b)
	s.dial = func(_ context.Context, a string) (bus, error) { addr = a; return b, nil }
	c := autoClient(t, s, nil)
	if c.Backend() != "dbus" || addr != "unix:path=/run/user/1000/bus" {
		t.Fatalf("backend=%s dialed %q", c.Backend(), addr)
	}
	if err := c.Notify(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(b.calls()) != 1 {
		t.Fatal("the toast must reach the notification server")
	}
}

func TestAutoShouldFallBackToXDGRuntimeDirBus(t *testing.T) {
	b := &fakeBus{}
	var addr string
	s := dbusSys(b)
	s.getenv = env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"})
	s.dial = func(_ context.Context, a string) (bus, error) { addr = a; return b, nil }
	if c := autoClient(t, s, nil); c.Backend() != "dbus" || addr != "unix:path=/run/user/1000/bus" {
		t.Fatalf("without DBUS_SESSION_BUS_ADDRESS the bus is $XDG_RUNTIME_DIR/bus: backend=%s dialed %q", c.Backend(), addr)
	}
}

func TestAutoShouldDisableAndWarnOnceWithoutServer(t *testing.T) {
	unowned := &fakeBus{noOwner: true}
	for name, s := range map[string]sys{
		"bus does not connect": {goos: "linux", getenv: env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x"}),
			dial: func(context.Context, string) (bus, error) {
				return nil, errors.New("dial unix /x: connect: no such file or directory")
			}},
		"nobody owns the name": {goos: "linux", getenv: env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x"}),
			dial: func(context.Context, string) (bus, error) { return unowned, nil }},
		"no bus at all (ssh, ci)": {goos: "linux", getenv: env(nil),
			dial: func(context.Context, string) (bus, error) { t.Fatal("nothing to dial"); return nil, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			var log syncBuffer
			c := autoClient(t, s, &log)
			for range 3 {
				if err := c.Notify(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"}); err != nil {
					t.Fatalf("no server is not an error: %v", err)
				}
			}
			if c.Backend() != "none" {
				t.Fatalf("backend = %s, want none", c.Backend())
			}
			if n := strings.Count(log.String(), "no desktop notification server; notifications disabled"); n != 1 {
				t.Fatalf("exactly one warning, got %d:\n%s", n, log.String())
			}
		})
	}
	if !unowned.isClosed() {
		t.Fatal("a bus without a notification server is closed")
	}
}

func TestMacOSBackendShouldBeDisabledWithoutAProgramToPostWith(t *testing.T) {
	x := &fakeExec{}
	s := sys{goos: "linux", getenv: env(nil), run: x.run,
		lookPath: func(string) (string, error) { return "", errors.New("not found") }}
	var log syncBuffer
	c := newWith(config.Notify{Enabled: true, Backend: "macos", Events: allEvents()}, Options{StateDir: t.TempDir(), Log: slogTo(&log)}, s)
	t.Cleanup(func() { _ = c.Close() })
	for range 3 {
		if err := c.Notify(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"}); err != nil {
			t.Fatalf("no sender is not an error: %v", err)
		}
	}
	if c.Backend() != "none" || len(x.calls) != 0 {
		t.Fatalf("macos outside a Mac sends nothing: backend=%s calls=%q", c.Backend(), x.calls)
	}
	if n := strings.Count(log.String(), "notifications disabled"); n != 1 || !strings.Contains(log.String(), "osascript") {
		t.Fatalf("exactly one warning naming the missing programs, got %d:\n%s", n, log.String())
	}
	if p := diagnose(context.Background(), "macos", s); p.Backend != "none" || !errors.Is(p.Err, errNoMacOS) {
		t.Fatalf("doctor must not report macos without a sender: %+v", p)
	}
}

func TestDiagnoseShouldReportTheServerAndItsActions(t *testing.T) {
	b := &fakeBus{caps: []string{"body", "actions"}, server: [2]string{"mako", "1.9.0"}}
	p := diagnose(context.Background(), "auto", dbusSys(b))
	if p.Backend != "dbus" || p.Server != "mako" || p.Version != "1.9.0" || !p.Actions {
		t.Fatalf("probe = %+v", p)
	}
	if !b.isClosed() {
		t.Fatal("the probe closes its bus")
	}

	x := &fakeExec{}
	if p := diagnose(context.Background(), "auto", macSys(x, true)); p.Backend != "macos" || p.TerminalNotifier != "/opt/homebrew/bin/terminal-notifier" {
		t.Fatalf("probe = %+v", p)
	}
	if p := diagnose(context.Background(), "auto", macSys(x, false)); p.Backend != "macos" || p.TerminalNotifier != "" {
		t.Fatalf("probe = %+v", p)
	}
	if p := diagnose(context.Background(), "auto", sys{goos: "linux", getenv: env(nil)}); p.Backend != "none" || p.Err == nil {
		t.Fatalf("probe = %+v", p)
	}
}

// silentBus is a session bus socket that accepts connections and never answers.
func silentBus(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "bus")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "unix:path=" + sock
}

func TestNewShouldGiveUpOnABusThatNeverAnswers(t *testing.T) {
	s := realSys()
	s.goos, s.getenv = "linux", env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": silentBus(t)})
	var log syncBuffer
	done := make(chan *Client, 1)
	go func() {
		done <- newWith(config.Notify{Enabled: true, Backend: "auto", Events: allEvents()},
			Options{StateDir: t.TempDir(), Log: slogTo(&log)}, s)
	}()
	select {
	case c := <-done:
		defer c.Close()
		if c.Backend() != "none" || strings.Count(log.String(), "no desktop notification server; notifications disabled") != 1 {
			t.Fatalf("a silent bus disables notifications with one warning: backend=%s\n%s", c.Backend(), log.String())
		}
	case <-time.After(8 * time.Second):
		t.Fatal("New is still blocked on a bus that never answers: the setup must give up after 5s")
	}
}

func TestAutoShouldBoundTheBusSetupToFiveSeconds(t *testing.T) {
	var dialDeadline, ownerDeadline time.Time
	var dialBounded, ownerBounded bool
	b := &fakeBus{onOwner: func(ctx context.Context) { ownerDeadline, ownerBounded = ctx.Deadline() }}
	s := dbusSys(b)
	s.dial = func(ctx context.Context, _ string) (bus, error) {
		dialDeadline, dialBounded = ctx.Deadline()
		return b, nil
	}
	start := time.Now()
	c := autoClient(t, s, nil)
	end := time.Now()
	if c.Backend() != "dbus" {
		t.Fatalf("backend = %s", c.Backend())
	}
	for name, d := range map[string]struct {
		at      time.Time
		bounded bool
	}{"dial": {dialDeadline, dialBounded}, "NameHasOwner": {ownerDeadline, ownerBounded}} {
		if !d.bounded || d.at.Before(start.Add(5*time.Second)) || d.at.After(end.Add(5*time.Second)) {
			t.Errorf("%s must run with a 5s deadline, got bounded=%v at %v after start", name, d.bounded, d.at.Sub(start))
		}
	}
}
