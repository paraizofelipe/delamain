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

func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// exists reports whether path is there; an error other than not-exist counts as there.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
