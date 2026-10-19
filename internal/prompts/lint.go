package prompts

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

var (
	referenceRe = regexp.MustCompile(`references/[A-Za-z0-9_-]+\.md`)
	nameRe      = regexp.MustCompile(Prefix + `[a-z0-9]+(?:-[a-z0-9]+)*`)
)

type frontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tools       []string `yaml:"tools"` // agents only
}

// Lint validates the tree a contributor edits. It returns every problem, not just the first.
func (s *Set) Lint() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	m := s.Manifest
	for _, kind := range []struct {
		dir    string
		listed []string
		files  bool
	}{{"roles", m.Roles, false}, {"agents", m.Agents, false}, {"skills", m.Skills, false}, {"stages", m.Stages, true}} {
		if len(kind.listed) == 0 {
			add("manifest: %s is empty", kind.dir)
		}
		onDisk, err := s.names(kind.dir, kind.files)
		if err != nil {
			add("%s: %v", kind.dir, err)
			continue
		}
		for _, n := range kind.listed {
			if !slices.Contains(onDisk, n) {
				add("manifest: %s/%s is listed but does not exist", kind.dir, n)
			}
		}
		for _, n := range onDisk {
			if !slices.Contains(kind.listed, n) {
				add("manifest: %s/%s exists but is not listed (it would never be used)", kind.dir, n)
			}
		}
	}

	known := map[string]bool{}
	for _, n := range m.Agents {
		known[Prefix+n] = true
	}
	for _, n := range m.Skills {
		known[Prefix+n] = true
	}

	bodies := map[string][]byte{}
	for _, n := range m.Roles {
		p := path.Join("roles", n, "body.md")
		b, err := fs.ReadFile(s.FS, p)
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			add("%s: missing or empty", p)
			continue
		}
		bodies[p] = b
	}
	for _, n := range m.Agents {
		dir := path.Join("agents", n)
		var fm frontmatter
		front, err := fs.ReadFile(s.FS, path.Join(dir, "omp.yml"))
		if err != nil {
			add("%s/omp.yml: %v", dir, err)
		} else if err := yaml.Unmarshal(front, &fm); err != nil {
			add("%s/omp.yml: %v", dir, err)
		} else {
			checkFrontmatter(add, dir+"/omp.yml", fm, Prefix+n)
			for _, tool := range fm.Tools {
				if _, ok := claudeTools[tool]; !ok {
					add("%s/omp.yml: tool %q is not one of %s", dir, tool, strings.Join(slices.Sorted(maps.Keys(claudeTools)), ", "))
				}
			}
		}
		b, err := fs.ReadFile(s.FS, path.Join(dir, "body.md"))
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			add("%s/body.md: missing or empty", dir)
			continue
		}
		bodies[dir+"/body.md"] = b
	}
	for _, n := range m.Skills {
		dir := path.Join("skills", n)
		p := path.Join(dir, "SKILL.md")
		b, err := fs.ReadFile(s.FS, p)
		if err != nil {
			add("%s: %v", p, err)
			continue
		}
		fm, err := parseFrontmatter(b)
		if err != nil {
			add("%s: %v", p, err)
		} else {
			checkFrontmatter(add, p, fm, Prefix+n)
		}
		bodies[p] = b
		walkErr := fs.WalkDir(s.FS, dir, func(fp string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || fp == p {
				return err
			}
			if rb, err := fs.ReadFile(s.FS, fp); err == nil {
				bodies[fp] = rb
			}
			return nil
		})
		if walkErr != nil {
			add("%s: %v", dir, walkErr)
		}
		// A reference a skill tells the agent to load must ship with that skill.
		for _, ref := range referenceRe.FindAllString(string(b), -1) {
			if _, err := fs.Stat(s.FS, path.Join(dir, ref)); err != nil {
				add("%s: mentions %s, which does not exist", p, ref)
			}
		}
	}

	sample := SampleStageData()
	for _, n := range m.Stages {
		p := path.Join("stages", n+".md")
		raw, err := fs.ReadFile(s.FS, p)
		if err != nil {
			continue // already reported by the manifest check
		}
		bodies[p] = raw
		out, err := s.Render(n, sample)
		if err != nil {
			add("%s: %v", p, err)
			continue
		}
		// The orchestrator only trusts artifacts: every stage must tell the agent where to write one.
		if !strings.Contains(out, sample.ArtifactPath) {
			add("%s: never references {{.ArtifactPath}}; the orchestrator could not verify the step", p)
		}
	}

	for _, p := range sortedKeys(bodies) {
		for _, ref := range nameRe.FindAllString(string(bodies[p]), -1) {
			if !known[ref] {
				add("%s: mentions %s, which is not an agent or skill in the manifest", p, ref)
			}
		}
	}
	return errs
}

func checkFrontmatter(add func(string, ...any), file string, fm frontmatter, wantName string) {
	if fm.Name != wantName {
		add("%s: name is %q, want %q", file, fm.Name, wantName)
	}
	if strings.TrimSpace(fm.Description) == "" {
		add("%s: description is required (omp uses it to decide when to apply)", file)
	}
}

func parseFrontmatter(b []byte) (frontmatter, error) {
	var fm frontmatter
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return fm, errors.New("missing frontmatter")
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---"))
	if !ok {
		return fm, errors.New("unterminated frontmatter")
	}
	err := yaml.Unmarshal(front, &fm)
	return fm, err
}

// names lists directory names (or *.md stems when files is true) under dir.
func (s *Set) names(dir string, files bool) ([]string, error) {
	entries, err := fs.ReadDir(s.FS, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		switch {
		case files && !e.IsDir() && strings.HasSuffix(e.Name(), ".md"):
			out = append(out, strings.TrimSuffix(e.Name(), ".md"))
		case !files && e.IsDir():
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
