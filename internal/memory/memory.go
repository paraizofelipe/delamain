// Package memory keeps per-repository review learnings under the state dir.
package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store keeps one markdown file of learnings per repository under Dir.
type Store struct {
	Dir string
}

// Path is the memory file of host/repoPath: <Dir>/<host>/<owner...>/<repo>.md.
// It returns "" when Dir is empty or host/repoPath could escape Dir.
func (s Store) Path(host, repoPath string) string {
	if s.Dir == "" || !validComponent(host) || filepath.IsAbs(repoPath) {
		return ""
	}
	parts := strings.Split(repoPath, "/")
	for _, c := range parts {
		if !validComponent(c) {
			return ""
		}
	}
	p := filepath.Join(append([]string{s.Dir, host}, parts...)...) + ".md"
	if rel, err := filepath.Rel(s.Dir, p); err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	return p
}

func validComponent(c string) bool {
	return c != "" && c != "." && c != ".." && !strings.ContainsAny(c, `/\`)
}

// Read returns the stored learnings of host/repoPath, or "" when there are none.
func (s Store) Read(host, repoPath string, max int) (string, error) {
	p := s.Path(host, repoPath)
	if p == "" {
		return "", ErrInvalidPath
	}
	b, err := os.ReadFile(p) //nolint:gosec // path is validated to stay under Dir
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// Append adds one dated line per learning to the memory of host/repoPath and
// returns how many were written.
func (s Store) Append(host, repoPath string, runID int64, pr int, learnings []string, now time.Time) (int, error) {
	p := s.Path(host, repoPath)
	if p == "" {
		return 0, ErrInvalidPath
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return 0, err
	}
	cur, err := os.ReadFile(p) //nolint:gosec // path is validated to stay under Dir
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	var b strings.Builder
	b.Write(cur)
	for _, l := range learnings {
		fmt.Fprintf(&b, "- %s (run %d, pr #%d): %s\n", now.Format("2006-01-02"), runID, pr, l)
	}
	if err := writeAtomic(p, []byte(b.String())); err != nil {
		return 0, err
	}
	return len(learnings), nil
}

func writeAtomic(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".memory-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// ErrInvalidPath reports a host or repository path that would escape Dir.
var ErrInvalidPath = errors.New("invalid memory path")
