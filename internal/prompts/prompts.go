// Package prompts loads, renders, validates and installs the .md prompts of delamain.
package prompts

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// Prefix namespaces installed omp agents and skills so they never collide with
// assets from other tools (e.g. catlog-agents also ships a `tdd` skill).
const Prefix = "delamain-"

type Manifest struct {
	Roles  []string `yaml:"roles"`
	Agents []string `yaml:"agents"`
	Skills []string `yaml:"skills"`
	Stages []string `yaml:"stages"`
}

// Set is a loaded prompt tree. All reads go through its file system, so override layers apply everywhere.
type Set struct {
	FS       fs.FS
	Manifest Manifest
}

// Load builds a Set from layers, highest precedence first (e.g. override dir, then embedded defaults).
func Load(layers ...fs.FS) (*Set, error) {
	fsys := Layered(layers)
	raw, err := fsys.ReadFile("manifest.yaml")
	if err != nil {
		return nil, fmt.Errorf("prompts manifest: %w", err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("prompts manifest: %w", err)
	}
	return &Set{FS: fsys, Manifest: m}, nil
}

// Role returns the body appended to a main session's system prompt.
func (s *Set) Role(name string) (string, error) {
	b, err := fs.ReadFile(s.FS, path.Join("roles", name, "body.md"))
	if err != nil {
		return "", fmt.Errorf("role %s: %w", name, err)
	}
	return string(b), nil
}

// agentParts reads an agent's parsed frontmatter and its body.
func (s *Set) agentParts(name string) (frontmatter, []byte, error) {
	dir := path.Join("agents", name)
	var fm frontmatter
	front, err := fs.ReadFile(s.FS, path.Join(dir, "omp.yml"))
	if err != nil {
		return fm, nil, fmt.Errorf("agent %s frontmatter: %w", name, err)
	}
	if err := yaml.Unmarshal(front, &fm); err != nil {
		return fm, nil, fmt.Errorf("agent %s frontmatter: %w", name, err)
	}
	body, err := fs.ReadFile(s.FS, path.Join(dir, "body.md"))
	if err != nil {
		return fm, nil, fmt.Errorf("agent %s body: %w", name, err)
	}
	return fm, body, nil
}

// Agent assembles an omp agent file: "---\n<omp.yml>\n---\n\n<body.md>".
// The frontmatter is passed through as opaque bytes, like catlog-agents' installer.
func (s *Set) Agent(name string) ([]byte, error) {
	dir := path.Join("agents", name)
	front, err := fs.ReadFile(s.FS, path.Join(dir, "omp.yml"))
	if err != nil {
		return nil, fmt.Errorf("agent %s frontmatter: %w", name, err)
	}
	body, err := fs.ReadFile(s.FS, path.Join(dir, "body.md"))
	if err != nil {
		return nil, fmt.Errorf("agent %s body: %w", name, err)
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(front)
	if !bytes.HasSuffix(front, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString("---\n\n")
	out.Write(body)
	return out.Bytes(), nil
}

type Issue struct {
	Ref   string
	Title string
	Body  string
	URL   string
	Kind  string // "feature" | "bug"
}

type Repo struct {
	Worktree string
	Branch   string
	Base     string
}

// StageData is everything a stage template may reference. Unknown fields fail rendering.
type StageData struct {
	Issue         Issue
	Repo          Repo
	Iteration     int
	MaxIterations int
	ArtifactPath  string
	PlanPath      string
	PlanJSONPath  string
	Feedback      string
	Verify        string
	// PrevReview digests the previous iteration's review so the next one does not move the bar.
	PrevReview string
	BudgetLeft string
}

// SampleStageData fills every field so lint can prove each template renders.
func SampleStageData() StageData {
	return StageData{
		Issue:         Issue{Ref: "#42", Title: "Sample issue", Body: "Body\n\n- [ ] criterion", URL: "https://example.invalid/issues/42", Kind: "feature"},
		Repo:          Repo{Worktree: "/tmp/wt", Branch: "feat/42-sample", Base: "origin/main"},
		Iteration:     2,
		MaxIterations: 3,
		ArtifactPath:  "/tmp/run/iter-2/artifact.json",
		PlanPath:      "/tmp/run/plan.md",
		PlanJSONPath:  "/tmp/run/plan.json",
		Feedback:      "- go test: TestX failed",
		Verify:        "lint ok · test 10 passed",
		PrevReview:    "- [minor · standards · julgamento] calc.go:7 — nome misterioso",
		BudgetLeft:    "1.2M tokens",
	}
}

// Delimiter tags wrapping untrusted text inside stage templates.
var delimiters = []string{"issue", "feedback", "verify", "prev_review"}

// neutralize stops untrusted text (issue bodies, tool output) from closing a delimiter
// tag early and smuggling instructions outside it.
func neutralize(s string) string {
	for _, tag := range delimiters {
		s = strings.ReplaceAll(s, "</"+tag, "&lt;/"+tag)
		s = strings.ReplaceAll(s, "<"+tag, "&lt;"+tag)
	}
	return s
}

// Render executes a stage template. Untrusted fields are neutralized first.
func (s *Set) Render(stage string, data StageData) (string, error) {
	raw, err := fs.ReadFile(s.FS, path.Join("stages", stage+".md"))
	if err != nil {
		return "", fmt.Errorf("stage %s: %w", stage, err)
	}
	tmpl, err := template.New(stage).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("stage %s: %w", stage, err)
	}
	data.Issue.Title = neutralize(data.Issue.Title)
	data.Issue.Body = neutralize(data.Issue.Body)
	data.Feedback = neutralize(data.Feedback)
	data.Verify = neutralize(data.Verify)
	data.PrevReview = neutralize(data.PrevReview)
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("stage %s: %w", stage, err)
	}
	return out.String(), nil
}
