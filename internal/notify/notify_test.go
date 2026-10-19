package notify

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
)

func TestIconShouldBeWrittenOnceToStateDirWithPermissions(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	want := filepath.Join(state, "notify", "delamain.png")

	path, err := writeIcon(state)
	if err != nil || path != want {
		t.Fatalf("icon path = %q, %v; want %q", path, err, want)
	}
	assertMode(t, filepath.Dir(want), os.ModeDir|0o700)
	assertMode(t, want, 0o644)
	if got, _ := os.ReadFile(want); !bytes.Equal(got, logo) || len(logo) == 0 {
		t.Fatal("the icon must be the embedded logo, unchanged")
	}

	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(want, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := writeIcon(state); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(want); !fi.ModTime().Equal(old) {
		t.Fatal("an icon with the same content must not be rewritten")
	}

	if err := os.WriteFile(want, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeIcon(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(want); !bytes.Equal(got, logo) {
		t.Fatal("an icon with other content must be overwritten with the logo")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode() != want {
		t.Fatalf("%s mode = %v, want %v", path, fi.Mode(), want)
	}
}

// env fakes the process environment.
func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// dialer hands out b and records the bus addresses dialed.
type dialer struct {
	b     *fakeBus
	err   error
	addrs []string
}

func (d *dialer) dial(addr string) (bus, error) {
	d.addrs = append(d.addrs, addr)
	if d.err != nil {
		return nil, d.err
	}
	return d.b, nil
}

func linuxOpts(t *testing.T, d *dialer, vars map[string]string) Options {
	return Options{StateDir: t.TempDir(), Log: discard(), goos: "linux", getenv: env(vars), dial: d.dial}
}

var sessionBus = map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}

func TestAutoShouldPickMacOSOnDarwin(t *testing.T) {
	d := &dialer{b: &fakeBus{owned: true}}
	o := linuxOpts(t, d, sessionBus)
	o.goos = "darwin"
	if got := BackendOf(New(config.Default().Notify, o)); got != "macos" || len(d.addrs) != 0 {
		t.Fatalf("darwin uses the Notification Center: backend=%q dialed=%v", got, d.addrs)
	}
}

func TestAutoShouldPickDBusWhenNotificationServerOwnsName(t *testing.T) {
	d := &dialer{b: &fakeBus{owned: true}}
	n := New(config.Default().Notify, linuxOpts(t, d, sessionBus))
	if got := BackendOf(n); got != "dbus" || !slices.Equal(d.addrs, []string{"unix:path=/run/user/1000/bus"}) {
		t.Fatalf("backend=%q dialed=%v", got, d.addrs)
	}
	if err := n.Notify(context.Background(), FormatPROpened(example())); err != nil {
		t.Fatal(err)
	}
	if got := d.b.last(t).Summary; got != "✅ PR opened · delamain #12" {
		t.Fatalf("the toast reaches the server, summary=%q", got)
	}
}

func TestAutoShouldFallBackToXDGRuntimeDirBus(t *testing.T) {
	d := &dialer{b: &fakeBus{owned: true}}
	n := New(config.Default().Notify, linuxOpts(t, d, map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}))
	if got := BackendOf(n); got != "dbus" || !slices.Equal(d.addrs, []string{"unix:path=/run/user/1000/bus"}) {
		t.Fatalf("without DBUS_SESSION_BUS_ADDRESS the bus is $XDG_RUNTIME_DIR/bus: backend=%q dialed=%v", got, d.addrs)
	}
}

func TestAutoShouldDisableAndWarnOnceWithoutServer(t *testing.T) {
	for name, d := range map[string]*dialer{
		"no owner":   {b: &fakeBus{owned: false}},
		"no bus":     {err: errors.New("dial unix /run/user/1000/bus: connect: no such file or directory")},
		"no address": {b: &fakeBus{owned: true}},
	} {
		t.Run(name, func(t *testing.T) {
			var log bytes.Buffer
			vars := sessionBus
			if name == "no address" {
				vars = nil // SSH or CI: neither DBUS_SESSION_BUS_ADDRESS nor XDG_RUNTIME_DIR
			}
			o := linuxOpts(t, d, vars)
			o.Log = slog.New(slog.NewTextHandler(&log, nil))
			n := New(config.Default().Notify, o)
			for range 3 {
				if err := n.Notify(context.Background(), FormatPROpened(example())); err != nil {
					t.Fatalf("no server never fails the caller: %v", err)
				}
			}
			if got := BackendOf(n); got != "none" {
				t.Fatalf("backend = %q", got)
			}
			if c := strings.Count(log.String(), "no desktop notification server; notifications disabled"); c != 1 || strings.Count(log.String(), "level=WARN") != 1 {
				t.Fatalf("want one warning, got %d:\n%s", c, log.String())
			}
		})
	}
}

func TestNotifyShouldTimeOutAfterFiveSeconds(t *testing.T) {
	b := &fakeBus{owned: true}
	start := time.Now()
	if err := New(config.Default().Notify, linuxOpts(t, &dialer{b: b}, sessionBus)).Notify(context.Background(), FormatPROpened(example())); err != nil {
		t.Fatal(err)
	}
	if left := b.deadline.Sub(start); left < 4*time.Second || left > 6*time.Second {
		t.Fatalf("the server gets 5s, deadline in %v", left)
	}

	// A server that ignores the deadline still cannot hold the caller.
	b = &fakeBus{owned: true, hang: make(chan struct{})}
	defer close(b.hang)
	var log bytes.Buffer
	o := linuxOpts(t, &dialer{b: b}, sessionBus)
	o.Log, o.timeout = slog.New(slog.NewTextHandler(&log, nil)), 50*time.Millisecond
	start = time.Now()
	if err := New(config.Default().Notify, o).Notify(context.Background(), FormatPROpened(example())); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second || !strings.Contains(log.String(), "context deadline exceeded") {
		t.Fatalf("returned after %v, log:\n%s", took, log.String())
	}
}

func TestNotifyErrorsShouldNeverFailTheCaller(t *testing.T) {
	var log bytes.Buffer
	o := linuxOpts(t, &dialer{b: &fakeBus{owned: true, err: errors.New("org.freedesktop.DBus.Error.ServiceUnknown")}}, sessionBus)
	o.Log = slog.New(slog.NewTextHandler(&log, nil))
	if err := New(config.Default().Notify, o).Notify(context.Background(), FormatHalted(example())); err != nil {
		t.Fatalf("a failed toast is only a warning: %v", err)
	}
	if !strings.Contains(log.String(), "level=WARN") || !strings.Contains(log.String(), "ServiceUnknown") || !strings.Contains(log.String(), "event=halted") {
		t.Fatalf("the failure is logged:\n%s", log.String())
	}
}

func TestDisabledConfigShouldResolveToNone(t *testing.T) {
	off := config.Default().Notify
	off.Enabled = false
	forcedNone := config.Default().Notify
	forcedNone.Backend = "none"
	for name, cfg := range map[string]config.Notify{"enabled=false": off, "backend=none": forcedNone} {
		d := &dialer{b: &fakeBus{owned: true}}
		if got := BackendOf(New(cfg, linuxOpts(t, d, sessionBus))); got != "none" || len(d.addrs) != 0 {
			t.Errorf("%s: backend=%q dialed=%v", name, got, d.addrs)
		}
	}
}
