package prompts

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Target says where installed prompts go.
type Target struct {
	// OmpAgentDir is omp's agent directory (default ~/.omp/agent); agents/ and skills/ live under it.
	OmpAgentDir string
	// RolesDir receives role bodies passed to omp via --append-system-prompt.
	RolesDir string
}

func (t Target) RolePath(role string) string { return filepath.Join(t.RolesDir, role+".md") }

// files computes every installed path and its content.
func (s *Set) files(t Target) (map[string][]byte, []string, error) {
	out := map[string][]byte{}
	var skillDirs []string
	for _, r := range s.Manifest.Roles {
		body, err := s.Role(r)
		if err != nil {
			return nil, nil, err
		}
		out[t.RolePath(r)] = []byte(body)
	}
	for _, a := range s.Manifest.Agents {
		data, err := s.Agent(a)
		if err != nil {
			return nil, nil, err
		}
		out[filepath.Join(t.OmpAgentDir, "agents", Prefix+a+".md")] = data
	}
	for _, sk := range s.Manifest.Skills {
		dst := filepath.Join(t.OmpAgentDir, "skills", Prefix+sk)
		skillDirs = append(skillDirs, dst)
		src := path.Join("skills", sk)
		err := fs.WalkDir(s.FS, src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(s.FS, p)
			if err != nil {
				return err
			}
			out[filepath.Join(dst, filepath.FromSlash(strings.TrimPrefix(p, src+"/")))] = data
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return out, skillDirs, nil
}

// Install writes roles, agents and skills, returning every written path (sorted).
// Only Prefix-named entries are replaced, so user assets in the same directories are never touched.
func (s *Set) Install(t Target) ([]string, error) {
	if errs := s.Lint(); len(errs) > 0 {
		return nil, fmt.Errorf("refusing to install invalid prompts (%d problems; run `delamain prompts lint`): %w", len(errs), errs[0])
	}
	files, skillDirs, err := s.files(t)
	if err != nil {
		return nil, err
	}
	// Replace whole skill dirs so references deleted upstream do not linger.
	for _, dir := range skillDirs {
		if !strings.HasPrefix(filepath.Base(dir), Prefix) {
			return nil, fmt.Errorf("refusing to replace %s: not a %s directory", dir, Prefix)
		}
		if err := os.RemoveAll(dir); err != nil {
			return nil, err
		}
	}
	paths := slices.Sorted(maps.Keys(files))
	for i, p := range paths {
		if err := writeAtomic(p, files[p]); err != nil {
			return paths[:i], err
		}
	}
	return paths, nil
}

// Drift lists installed files that are missing or differ from the source.
func (s *Set) Drift(t Target) (missing, outdated []string, err error) {
	files, _, err := s.files(t)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		cur, err := os.ReadFile(p) //nolint:gosec // paths are built from the manifest under the target dirs
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, p)
		case err != nil:
			return nil, nil, err
		case !bytes.Equal(cur, files[p]):
			outdated = append(outdated, p)
		}
	}
	return missing, outdated, nil
}

func writeAtomic(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".delamain-*")
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
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
