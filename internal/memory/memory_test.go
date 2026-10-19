package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

func TestPathShouldRejectTraversalAndAbsolutePaths(t *testing.T) {
	s := newStore(t)
	cases := []struct{ host, repo string }{
		{"github.com", "../../etc/passwd"},
		{"github.com", "acme/../../x"},
		{"github.com", "acme/.."},
		{"github.com", "./app"},
		{"github.com", "/etc/passwd"},
		{"github.com", "acme//app"},
		{"github.com", "acme/app/"},
		{"github.com", ""},
		{"github.com", `acme\..\app`},
		{"..", "acme/app"},
		{".", "acme/app"},
		{"", "acme/app"},
		{"github.com/../..", "acme/app"},
		{`github.com\x`, "acme/app"},
	}
	for _, c := range cases {
		if p := s.Path(c.host, c.repo); p != "" {
			t.Errorf("Path(%q, %q) = %q, want rejection", c.host, c.repo, p)
		}
		if _, err := s.Read(c.host, c.repo, 1000); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Read(%q, %q) err = %v, want ErrInvalidPath", c.host, c.repo, err)
		}
		if n, err := s.Append(c.host, c.repo, 1, 1, []string{"x"}, day); !errors.Is(err, ErrInvalidPath) || n != 0 {
			t.Errorf("Append(%q, %q) = %d, %v, want 0, ErrInvalidPath", c.host, c.repo, n, err)
		}
	}
	if _, err := os.Stat(s.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected paths must not touch the disk, stat %s: %v", s.Dir, err)
	}

	if p := (Store{}).Path("github.com", "acme/app"); p != "" {
		t.Errorf("empty Dir must be rejected, got %q", p)
	}
	p := s.Path("github.com", "acme/app")
	if rel, err := filepath.Rel(s.Dir, p); err != nil || !filepath.IsLocal(rel) {
		t.Fatalf("valid path %q must stay under %q", p, s.Dir)
	}
}

func TestFilesShouldBeCreatedWithPrivatePermissions(t *testing.T) {
	s := newStore(t)
	if _, err := s.Append("github.com", "acme/app", 1, 1, []string{"keep it private"}, day); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{s.Dir, filepath.Join(s.Dir, "github.com"), filepath.Join(s.Dir, "github.com", "acme")} {
		assertMode(t, d, 0o700)
	}
	p := s.Path("github.com", "acme/app")
	assertMode(t, p, 0o600)
	assertMode(t, p+".lock", 0o600)
}

func assertMode(t *testing.T, p string, want fs.FileMode) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", p, got, want)
	}
}

func TestAppendShouldDiscardLearningsOver300Chars(t *testing.T) {
	s := newStore(t)
	exact := strings.Repeat("é", 300) // 300 runes, 600 bytes: the limit counts characters
	over := strings.Repeat("a", 301)
	n, err := s.Append("github.com", "acme/app", 1, 1, []string{over, "  " + exact + "  ", "short"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("wrote %d, want 2", n)
	}
	want := "- 2026-10-19 (run 1, pr #1): " + exact + "\n- 2026-10-19 (run 1, pr #1): short\n"
	if got := readFile(t, s.Path("github.com", "acme/app")); got != want {
		t.Fatalf("over-limit learning must be dropped, not truncated:\n%s", got)
	}
}

func TestAppendShouldKeepAtMost10PerCall(t *testing.T) {
	s := newStore(t)
	var in []string
	for i := 1; i <= 11; i++ {
		in = append(in, fmt.Sprintf("learning %d", i))
	}
	n, err := s.Append("github.com", "acme/app", 1, 1, in, day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("wrote %d, want 10", n)
	}
	got := readFile(t, s.Path("github.com", "acme/app"))
	if c := strings.Count(got, "\n"); c != 10 {
		t.Fatalf("file has %d lines, want 10:\n%s", c, got)
	}
	if !strings.Contains(got, "learning 10\n") || strings.Contains(got, "learning 11") {
		t.Fatalf("must keep the first 10 in order:\n%s", got)
	}
}
