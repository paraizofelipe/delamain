package prompts

import (
	"bytes"
	"encoding/json"
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

// Harnesses the prompts are installed for; empty means omp.
const (
	HarnessOmp    = "omp"
	HarnessClaude = "claude"
)

// Target says where installed prompts go.
type Target struct {
	// Harness picks the layout of agents and skills: omp's agent dir or claude's files.
	Harness string
	// OmpAgentDir is omp's agent directory (default ~/.omp/agent); agents/ and skills/ live under it.
	OmpAgentDir string
	// RolesDir receives role bodies appended to the harness' system prompt.
	RolesDir string
	// ClaudeDir receives agents.json (claude --agents) and plugin/ (claude --plugin-dir, the skills).
	ClaudeDir string
	// Agents sets the model and reasoning of subagents, keyed by their manifest name.
	Agents map[string]AgentModel
}

// AgentModel overrides a subagent's model and reasoning (omp's thinking-level, claude's
// effort); an empty field keeps the subagent's own setting or the harness default.
type AgentModel struct {
	Model, Reasoning string
}

func (t Target) RolePath(role string) string { return filepath.Join(t.RolesDir, role+".md") }

// AgentsFile is the subagents definition passed to claude --agents.
func (t Target) AgentsFile() string { return filepath.Join(t.ClaudeDir, "agents.json") }

// PluginDir is the plugin passed to claude --plugin-dir; it carries the skills.
func (t Target) PluginDir() string { return filepath.Join(t.ClaudeDir, "plugin") }

// claudeTools maps the tool names agents declare in omp.yml to Claude Code's.
var claudeTools = map[string]string{"read": "Read", "grep": "Grep", "glob": "Glob", "lsp": "LSP", "bash": "Bash"}

// files computes every installed path and its content.
func (s *Set) files(t Target) (map[string][]byte, []string, error) {
	out := map[string][]byte{}
	for _, r := range s.Manifest.Roles {
		body, err := s.Role(r)
		if err != nil {
			return nil, nil, err
		}
		out[t.RolePath(r)] = []byte(body)
	}
	skillsDir := filepath.Join(t.OmpAgentDir, "skills")
	if t.Harness == HarnessClaude {
		agents, err := s.claudeAgents(t.Agents)
		if err != nil {
			return nil, nil, err
		}
		out[t.AgentsFile()] = agents
		out[filepath.Join(t.PluginDir(), ".claude-plugin", "plugin.json")] = []byte(`{"name":"delamain","description":"delamain's skills for its agents"}` + "\n")
		skillsDir = filepath.Join(t.PluginDir(), "skills")
	} else {
		for _, a := range s.Manifest.Agents {
			data, err := s.agentFile(a, t.Agents[a])
			if err != nil {
				return nil, nil, err
			}
			out[filepath.Join(t.OmpAgentDir, "agents", Prefix+a+".md")] = data
		}
	}
	skillDirs, err := s.skillFiles(skillsDir, out)
	return out, skillDirs, err
}

// claudeAgents renders every agent as a claude --agents definition, keyed by its name.
func (s *Set) claudeAgents(models map[string]AgentModel) ([]byte, error) {
	type def struct {
		Description string   `json:"description"`
		Prompt      string   `json:"prompt"`
		Tools       []string `json:"tools,omitempty"`
		Model       string   `json:"model,omitempty"`
		Effort      string   `json:"effort,omitempty"`
	}
	defs := map[string]def{}
	for _, a := range s.Manifest.Agents {
		fm, body, err := s.agentParts(a)
		if err != nil {
			return nil, err
		}
		m := models[a]
		d := def{Description: fm.Description, Prompt: string(body), Model: m.Model, Effort: m.Reasoning}
		for _, tool := range fm.Tools {
			d.Tools = append(d.Tools, claudeTools[tool])
		}
		defs[fm.Name] = d
	}
	b, err := json.MarshalIndent(defs, "", "  ")
	return append(b, '\n'), err
}

// skillFiles adds every skill's files under dir/delamain-<skill>/ and returns those dirs.
func (s *Set) skillFiles(dir string, out map[string][]byte) ([]string, error) {
	var skillDirs []string
	for _, sk := range s.Manifest.Skills {
		dst := filepath.Join(dir, Prefix+sk)
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
			return nil, err
		}
	}
	return skillDirs, nil
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
