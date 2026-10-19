package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var day = time.Date(2026, 10, 19, 15, 4, 5, 0, time.UTC)

func newStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "memory")}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadShouldReturnEmptyWhenFileDoesNotExist(t *testing.T) {
	s := newStore(t)
	got, err := s.Read("github.com", "acme/app", 1000)
	if err != nil {
		t.Fatalf("missing memory must not be an error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestAppendShouldWriteDatedLinesWithRunAndPR(t *testing.T) {
	s := newStore(t)
	n, err := s.Append("gitlab.com", "group/sub/app", 42, 7, []string{"use table tests", "wrap errors with %w"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("wrote %d, want 2", n)
	}
	want := filepath.Join(s.Dir, "gitlab.com", "group", "sub", "app.md")
	if got := s.Path("gitlab.com", "group/sub/app"); got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	body := readFile(t, want)
	if body != "- 2026-10-19 (run 42, pr #7): use table tests\n- 2026-10-19 (run 42, pr #7): wrap errors with %w\n" {
		t.Fatalf("unexpected file body:\n%s", body)
	}
	if _, err := s.Append("gitlab.com", "group/sub/app", 43, 8, []string{"prefer early returns"}, day.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, want); got != body+"- 2026-10-20 (run 43, pr #8): prefer early returns\n" {
		t.Fatalf("second append must go at the end:\n%s", got)
	}
	entries, _ := filepath.Glob(filepath.Join(filepath.Dir(want), "*"))
	for _, e := range entries {
		if b := filepath.Base(e); b != "app.md" && b != "app.md.lock" {
			t.Fatalf("leftover temp file %s", e)
		}
	}
}
