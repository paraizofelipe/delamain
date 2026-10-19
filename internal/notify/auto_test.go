package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	s.dial = func(string) (bus, error) { dialed = true; return &fakeBus{}, nil }
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
	s.dial = func(a string) (bus, error) { addr = a; return b, nil }
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
	s.dial = func(a string) (bus, error) { addr = a; return b, nil }
	if c := autoClient(t, s, nil); c.Backend() != "dbus" || addr != "unix:path=/run/user/1000/bus" {
		t.Fatalf("without DBUS_SESSION_BUS_ADDRESS the bus is $XDG_RUNTIME_DIR/bus: backend=%s dialed %q", c.Backend(), addr)
	}
}

func TestAutoShouldDisableAndWarnOnceWithoutServer(t *testing.T) {
	unowned := &fakeBus{noOwner: true}
	for name, s := range map[string]sys{
		"bus does not connect": {goos: "linux", getenv: env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x"}),
			dial: func(string) (bus, error) { return nil, errors.New("dial unix /x: connect: no such file or directory") }},
		"nobody owns the name": {goos: "linux", getenv: env(map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/x"}),
			dial: func(string) (bus, error) { return unowned, nil }},
		"no bus at all (ssh, ci)": {goos: "linux", getenv: env(nil),
			dial: func(string) (bus, error) { t.Fatal("nothing to dial"); return nil, nil }},
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
