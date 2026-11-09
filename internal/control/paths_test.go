package control

import (
	"os"
	"path/filepath"
	"testing"
)

// A state dir reached through a symlink must still contain the run dirs under it once they are
// gone: rm of a run whose dir was already removed would otherwise be refused as outside it.
func TestWithinShouldFollowTheSymlinksOfAPathThatIsGone(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir, path string
		want      bool
	}{
		{filepath.Join(link, "state"), filepath.Join(link, "state", "runs", "42"), true},
		{filepath.Join(real, "state"), filepath.Join(link, "state", "runs", "42"), true},
		{filepath.Join(link, "state"), filepath.Join(real, "state", "runs", "42"), true},
		{filepath.Join(link, "state"), filepath.Join(link, "other", "runs", "42"), false},
		{filepath.Join(link, "state"), filepath.Join(link, "state"), false},
		{filepath.Join(link, "state"), filepath.Join(link, "state", "..", "escape"), false},
	} {
		if got := within(c.dir, c.path); got != c.want {
			t.Fatalf("within(%q, %q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}
