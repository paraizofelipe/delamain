package prompts

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
)

// Layered is a read-only union of file systems: the first layer that has a path wins.
// Directory listings are merged so an override layer can add files without copying the rest.
type Layered []fs.FS

func (l Layered) Open(name string) (fs.File, error) {
	for _, layer := range l {
		f, err := layer.Open(name)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (l Layered) ReadFile(name string) ([]byte, error) {
	for _, layer := range l {
		b, err := fs.ReadFile(layer, name)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
}

func (l Layered) ReadDir(name string) ([]fs.DirEntry, error) {
	seen := map[string]bool{}
	var out []fs.DirEntry
	found := false
	for _, layer := range l {
		entries, err := fs.ReadDir(layer, name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = true
		for _, e := range entries {
			if !seen[e.Name()] {
				seen[e.Name()] = true
				out = append(out, e)
			}
		}
	}
	if !found {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}
