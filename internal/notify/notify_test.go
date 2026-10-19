package notify

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
