package notify

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
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
