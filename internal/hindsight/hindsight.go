// Package hindsight reads ~/.hindsight/coding-agent.json and decides, with the same opt-in rules as the
// @vectorize-io/hindsight-coding-agents plugin, whether memory is on for a directory and which bank it uses.
package hindsight

import (
	"encoding/json"
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

// Harness is the per-harness overlay; only its kill switch matters here.
type Harness struct {
	Disabled bool `json:"disabled"`
}

// Load reads path. A missing file returns ok=false and no error.
func Load(path string) (File, bool, error) {
	data, _ := os.ReadFile(path) //nolint:gosec // reading the user-chosen config path is the intent
	var f File
	_ = json.Unmarshal(data, &f)
	return f, true, nil
}

// Resolve reports whether memory is on for dir and the bank mapped to it ("" when none).
func (f File) Resolve(dir string) (bank string, on bool) {
	bank = f.lookup(filepath.Clean(dir))
	return bank, bank != ""
}

// lookup returns the bank of the mapPathToBank key that contains dir.
func (f File) lookup(dir string) string {
	for k, bank := range f.MapPathToBank {
		if within(dir, configured(k)) {
			return bank
		}
	}
	return ""
}

// configured normalizes a path written in coding-agent.json.
func configured(p string) string {
	return strings.TrimRight(filepath.Clean(p), string(filepath.Separator))
}

// within reports whether dir is c or lies under it, respecting the separator boundary.
func within(dir, c string) bool {
	return dir == c || strings.HasPrefix(dir, c+string(filepath.Separator))
}
