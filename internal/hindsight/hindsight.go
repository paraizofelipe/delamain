// Package hindsight reads ~/.hindsight/coding-agent.json and decides, with the same opt-in rules as the
// @vectorize-io/hindsight-coding-agents plugin, whether memory is on for a directory and which bank it uses.
package hindsight

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File holds only the decision fields of coding-agent.json; the apiToken is never decoded.
type File struct {
	Disabled      bool               `json:"disabled"`
	OptInOnly     bool               `json:"optInOnly"`
	OptInPaths    []string           `json:"optInPaths"`
	MapPathToBank map[string]string  `json:"mapPathToBank"`
	Harnesses     map[string]Harness `json:"harnesses"`
}

// harness is the key of this agent in coding-agent.json "harnesses".
const harness = "claude-code"

// Harness is the per-harness overlay; only its kill switch matters here.
type Harness struct {
	Disabled bool `json:"disabled"`
}

// Load reads path. A missing file returns ok=false and no error; errors never carry the file content.
func Load(path string) (File, bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // reading the user-chosen config path is the intent
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return f, true, nil
}

// Resolve reports whether memory is on for dir and the bank mapped to it ("" when none).
// disabled, global or for claude-code, turns memory off; under optInOnly, memory is on only inside
// optInPaths or a mapPathToBank entry with a non-empty bank.
func (f File) Resolve(dir string) (bank string, on bool) {
	if f.Disabled || f.Harnesses[harness].Disabled {
		return "", false
	}
	if dir == "" {
		return "", !f.OptInOnly
	}
	dir = filepath.Clean(dir)
	bank = f.lookup(dir)
	if !f.OptInOnly || bank != "" {
		return bank, true
	}
	for _, p := range f.OptInPaths {
		if within(dir, configured(p)) {
			return "", true
		}
	}
	return "", false
}

// lookup returns the bank of the longest mapPathToBank key that contains dir.
func (f File) lookup(dir string) string {
	bank, longest := "", -1
	for k, b := range f.MapPathToBank {
		if c := configured(k); within(dir, c) && len(c) > longest {
			bank, longest = b, len(c)
		}
	}
	return bank
}

// configured normalizes a path written in coding-agent.json: expands ~, cleans and drops trailing separators.
func configured(p string) string {
	return strings.TrimRight(filepath.Clean(expandHome(p)), string(filepath.Separator))
}

// expandHome expands ~ and ~/; without a home directory the path is kept as is, so it matches nothing.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[1:])
}

// within reports whether dir is c or lies under it, respecting the separator boundary.
func within(dir, c string) bool {
	return dir == c || strings.HasPrefix(dir, c+string(filepath.Separator))
}
