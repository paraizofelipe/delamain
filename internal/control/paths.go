package control

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// within reports whether path is inside dir, following symlinks such as /tmp on macOS.
func within(dir, path string) bool {
	rel, err := filepath.Rel(resolve(dir), resolve(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool { return b != "" && resolve(a) == resolve(b) }

// resolve follows the symlinks of p. A path that is gone, such as a run dir already removed,
// has its nearest existing ancestor resolved instead, so it still compares with a dir reached
// through a symlink.
func resolve(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	for dir, rest := p, ""; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(r, rest)
		}
		dir = parent
	}
}

// exists reports whether path is there; an error other than not-exist counts as there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
