package notify

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
)

func TestIconShouldBeWrittenOnceToStateDirWithPermissions(t *testing.T) {
	logo, err := os.ReadFile("delamain.png")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	notifyOnce := func() {
		t.Helper()
		c := newDBusClient(t, &fakeBus{}, Options{StateDir: state})
		if err := c.Notify(context.Background(), Message{Event: EventPROpened, RunID: 1, Title: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	notifyOnce()
	dir, path := filepath.Join(state, "notify"), filepath.Join(state, "notify", "delamain.png")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("icon dir must be 0700: %v %v", fi, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("icon must be 0644: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, logo) {
		t.Fatal("the written icon must be the embedded logo, unchanged")
	}

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	notifyOnce()
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Fatalf("an identical icon must not be rewritten (mtime %v → %v)", old, fi.ModTime())
	}

	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	notifyOnce()
	if got, _ := os.ReadFile(path); !bytes.Equal(got, logo) {
		t.Fatal("a different icon must be overwritten with the embedded logo")
	}
}

func TestDisabledEventsShouldNotNotify(t *testing.T) {
	b := &fakeBus{}
	c := newWith(config.Notify{Enabled: true, Backend: "dbus", Events: []string{"halted"}}, Options{StateDir: t.TempDir(), Log: discardLog()}, dbusSys(b))
	ctx := context.Background()
	for _, e := range []Event{EventStarted, EventPROpened, EventHalted, EventBudget} {
		if err := c.Notify(ctx, Message{Event: e, RunID: 1, Title: string(e)}); err != nil {
			t.Fatal(err)
		}
	}
	calls := b.calls()
	if len(calls) != 1 || calls[0].Summary != "halted" {
		t.Fatalf("only the enabled event goes out, got %+v", calls)
	}
}

func TestDisabledNotifyShouldSendNothing(t *testing.T) {
	b := &fakeBus{}
	dialed := false
	s := dbusSys(b)
	s.dial = func(string) (bus, error) { dialed = true; return b, nil }
	c := newWith(config.Notify{Enabled: false, Backend: "dbus", Events: allEvents()}, Options{StateDir: t.TempDir(), Log: discardLog()}, s)
	if err := c.Notify(context.Background(), Message{Event: EventHalted, RunID: 1, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if dialed || len(b.calls()) != 0 {
		t.Fatalf("disabled notifications never touch the bus: dialed=%v sent=%d", dialed, len(b.calls()))
	}
}
