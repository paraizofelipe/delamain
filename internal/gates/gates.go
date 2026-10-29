// Package gates holds the orchestrator's deterministic quality rules: which commands prove a
// repository is healthy and which diff shapes are rejected regardless of what agents claim.
package gates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/config"
)

// Red-proof policies: when new tests must be shown failing on the base branch.
const (
	RedProofBug    = "bug"
	RedProofAlways = "always"
	RedProofNever  = "never"
)

// Stack is what Detect recognized in a repository.
type Stack struct {
	Name     string // go | node | python | "" (unknown)
	Commands artifacts.Commands
}

// Detect derives the standard commands of a repository from its manifest files.
func Detect(dir string) Stack {
	exists := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	switch {
	case exists("go.mod"):
		return Stack{Name: "go", Commands: artifacts.Commands{
			Test: "go test ./...", Lint: "go vet ./...", FormatCheck: `test -z "$(gofmt -l .)"`,
		}}
	case exists("package.json"):
		return Stack{Name: "node", Commands: nodeCommands(dir, exists)}
	case exists("pyproject.toml") || exists("setup.py") || exists("requirements.txt"):
		return Stack{Name: "python", Commands: pythonCommands(dir, exists)}
	}
	return Stack{}
}

func nodeCommands(dir string, exists func(string) bool) artifacts.Commands {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // repository manifest
	_ = json.Unmarshal(raw, &pkg)
	pm := "npm"
	for _, l := range [][2]string{{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"bun.lockb", "bun"}, {"bun.lock", "bun"}} {
		if exists(l[0]) {
			pm = l[1]
			break
		}
	}
	script := func(names ...string) string {
		for _, n := range names {
			if s, ok := pkg.Scripts[n]; ok && !strings.Contains(s, "no test specified") {
				return pm + " run " + n
			}
		}
		return ""
	}
	return artifacts.Commands{
		Test: script("test"), Lint: script("lint"), Typecheck: script("typecheck", "type-check", "tsc"),
		FormatCheck: script("format:check", "format-check", "fmt:check"),
	}
}

func pythonCommands(dir string, exists func(string) bool) artifacts.Commands {
	run := ""
	if exists("uv.lock") {
		run = "uv run "
	}
	pyproject, _ := os.ReadFile(filepath.Join(dir, "pyproject.toml")) //nolint:gosec // repository manifest
	tool := func(name string) bool {
		return bytes.Contains(pyproject, []byte("[tool."+name+"]")) || exists(name+".toml") || exists("."+name+".toml")
	}
	c := artifacts.Commands{Test: run + "pytest -q"}
	if tool("ruff") {
		c.Lint, c.FormatCheck = run+"ruff check .", run+"ruff format --check ."
	}
	if tool("mypy") || exists("mypy.ini") {
		c.Typecheck = run + "mypy ."
	}
	return c
}

// Effective merges commands per field: the repository's .delamain.yaml wins, then detection;
// the planner's proposal only fills gaps, since an agent could otherwise pick `true` as its test.
// sources tells, per field, where the command came from.
func Effective(repo, detected, plan artifacts.Commands) (artifacts.Commands, map[string]string) {
	sources := map[string]string{}
	pick := func(name, r, d, p string) string {
		for _, c := range []struct{ src, v string }{{"repo", r}, {"detectado", d}, {"planner", p}} {
			if strings.TrimSpace(c.v) != "" {
				sources[name] = c.src
				return c.v
			}
		}
		return ""
	}
	return artifacts.Commands{
		Test:        pick("test", repo.Test, detected.Test, plan.Test),
		TestFile:    pick("test_file", repo.TestFile, detected.TestFile, plan.TestFile),
		Lint:        pick("lint", repo.Lint, detected.Lint, plan.Lint),
		Typecheck:   pick("typecheck", repo.Typecheck, detected.Typecheck, plan.Typecheck),
		FormatCheck: pick("format_check", repo.FormatCheck, detected.FormatCheck, plan.FormatCheck),
	}, sources
}

// RepoConfig is the target repository's .delamain.yaml.
type RepoConfig struct {
	Commands artifacts.Commands `yaml:"commands"`
	Gates    RepoGates          `yaml:"gates"`
	// Agent, Claude, Omp and Agents override the user config for this repository only: a
	// non-zero field wins, an explicitly empty list or map clears the global one (see
	// config.WithProjectOverrides). Read from the user's own clone, never from a PR's head.
	Agent  config.Agent                    `yaml:"agent"`
	Claude config.Claude                   `yaml:"claude"`
	Omp    config.Omp                      `yaml:"omp"`
	Agents map[string]config.AgentSettings `yaml:"agents"`
}

type RepoGates struct {
	RedProof     string `yaml:"red_proof"`
	RequireTests *bool  `yaml:"require_tests"`
	// SubjectLimit caps a commit subject in characters; 0 means DefaultSubjectLimit.
	SubjectLimit int `yaml:"subject_limit"`
}

const RepoConfigFile = ".delamain.yaml"

// LoadRepoConfig reads dir/.delamain.yaml; a missing file is an empty config.
func LoadRepoConfig(dir string) (RepoConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RepoConfigFile)) //nolint:gosec // repository config
	if errors.Is(err, os.ErrNotExist) {
		return RepoConfig{}, nil
	}
	if err != nil {
		return RepoConfig{}, err
	}
	return ParseRepoConfig(raw)
}

// ParseRepoConfig decodes a .delamain.yaml strictly (unknown keys fail); empty input is an
// empty config. It serves configs read elsewhere, e.g. from a git ref.
func ParseRepoConfig(raw []byte) (RepoConfig, error) {
	var rc RepoConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&rc); errors.Is(err, io.EOF) {
		return RepoConfig{}, nil
	} else if err != nil {
		return rc, fmt.Errorf("%s: %w", RepoConfigFile, err)
	}
	if err := ValidRedProof(rc.Gates.RedProof); rc.Gates.RedProof != "" && err != nil {
		return rc, fmt.Errorf("%s: %w", RepoConfigFile, err)
	}
	return rc, nil
}

func ValidRedProof(p string) error {
	switch p {
	case RedProofBug, RedProofAlways, RedProofNever:
		return nil
	}
	return fmt.Errorf("red_proof %q não é bug|always|never", p)
}

var (
	testName = regexp.MustCompile(`(^|/)(test_[^/]+\.py|[^/]+_test\.(go|py)|[^/]+\.(test|spec)\.[jt]sx?|[^/]+_spec\.rb)$`)
	testDir  = regexp.MustCompile(`(^|/)(__tests__|tests?|spec)/`)
	codeExt  = map[string]bool{
		".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".mjs": true, ".cjs": true,
		".rb": true, ".java": true, ".kt": true, ".rs": true, ".cs": true, ".php": true, ".swift": true, ".scala": true,
		".vue": true, ".svelte": true, ".c": true, ".cc": true, ".cpp": true, ".h": true, ".sql": true, ".sh": true,
	}
)

// IsTest recognizes test files by the usual naming and directory conventions.
func IsTest(file string) bool {
	return testName.MatchString(file) || testDir.MatchString(file)
}

// IsProduction is source code that is not a test: what a behavior change touches.
func IsProduction(file string) bool { return codeExt[path.Ext(file)] && !IsTest(file) }

// DefaultSubjectLimit is git's conventional soft cap for a commit subject. A repository that
// documents a stricter one sets gates.subject_limit in .delamain.yaml.
const DefaultSubjectLimit = 72

// Commits fails when a commit subject is longer than limit (0 = DefaultSubjectLimit). Measuring
// this here keeps it off the reviewer's plate: it costs an iteration to have an LLM report what a
// rule can decide, and run 12 of this repository spent one on a 73-character subject.
// Each entry comes from Git.CommitsSince as "<short sha> <subject>".
func Commits(commits []string, limit int) (bool, string) {
	if limit <= 0 {
		limit = DefaultSubjectLimit
	}
	var long []string
	for _, c := range commits {
		sha, subject, ok := strings.Cut(c, " ")
		if !ok {
			continue
		}
		if n := utf8.RuneCountInString(subject); n > limit {
			long = append(long, fmt.Sprintf("%s: assunto com %d caracteres (limite %d): %s", sha, n, limit, subject))
		}
	}
	if len(long) == 0 {
		return true, ""
	}
	return false, strings.Join(long, "\n")
}

// prTitleHeader is the commitlint header shape: a known type, an optional lowercase scope, an
// optional "!" and a description after ": ".
var prTitleHeader = regexp.MustCompile(`^(feat|fix|chore|docs|refactor|test|ci|perf|build|style)(\([a-z0-9._-]+\))?!?: \S`)

// PRTitle checks the PR/MR title the implementer proposes (Impl.PRTitle) against commitlint in
// English, so a malformed title fails here instead of costing a review iteration.
func PRTitle(title string, limit int) (bool, string) {
	if strings.TrimSpace(title) == "" {
		return false, "título do PR vazio"
	}
	var bad []string
	if !prTitleHeader.MatchString(title) {
		bad = append(bad, "título do PR fora do Conventional Commits (tipo(escopo)!: descrição): "+title)
	}
	if len(bad) == 0 {
		return true, ""
	}
	return false, strings.Join(bad, "\n")
}

// TestsTouched fails when production code changed but no test file did.
func TestsTouched(changed []string) (bool, string) {
	var prod []string
	for _, f := range changed {
		if IsTest(f) {
			return true, ""
		}
		if IsProduction(f) {
			prod = append(prod, f)
		}
	}
	if len(prod) == 0 {
		return true, ""
	}
	return false, "código de produção alterado sem nenhum arquivo de teste alterado: " + strings.Join(prod, ", ")
}

// Matrix checks the plan's criterion → test map against the worktree: every referenced test file
// exists inside it and contains the test's name; in a bug, regression tests must be in the diff.
func Matrix(dir string, plan artifacts.Plan, changed []string) []string {
	var problems []string
	check := func(owner, ref string) (file string, ok bool) {
		file, name, _ := strings.Cut(ref, "::")
		file = path.Clean(strings.TrimSpace(file))
		if file == "." || path.IsAbs(file) || strings.HasPrefix(file, "../") {
			problems = append(problems, fmt.Sprintf("%s: caminho de teste inválido %q", owner, ref))
			return "", false
		}
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file))) //nolint:gosec // confined to the worktree above
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: arquivo de teste %s não existe", owner, file))
			return file, false
		}
		for seg := range strings.SplitSeq(strings.TrimSpace(name), "/") {
			if seg == "" || bytes.Contains(raw, []byte(seg)) || bytes.Contains(raw, []byte(strings.ReplaceAll(seg, "_", " "))) {
				continue
			}
			problems = append(problems, fmt.Sprintf("%s: teste %q não encontrado em %s", owner, seg, file))
			return file, false
		}
		return file, true
	}
	for _, a := range plan.Acceptance {
		for _, ref := range a.Tests {
			check("critério "+strconv.Quote(a.Criterion), ref)
		}
	}
	if plan.Kind == "bug" {
		for _, ref := range plan.RegressionTests {
			if file, ok := check("teste de regressão", ref); ok && !slices.Contains(changed, file) {
				problems = append(problems, fmt.Sprintf("teste de regressão %s não foi criado nem alterado nesta branch", file))
			}
		}
	}
	return problems
}

var (
	// legacyLine is the old free-form declaration, read only at the start of a line, optionally
	// inside quotes and after a list marker and a checkbox. Its list of issues ("#1, #2 e #3")
	// ends at the first token that is not a reference, a comma, "e" or "and".
	legacyLine = regexp.MustCompile(`(?i)^[ \t]*(?:>[ \t]*)*(?:(?:[-*+]|[0-9]+\.)[ \t]+)?(?:\[[ x]\][ \t]+)?(?:blocked by|depends on|depende d(?:as|os|e|a|o)|bloquead[ao] por)[ \t]+(#[0-9]+(?:(?:[ \t]*,[ \t]*|[ \t]+(?:e|and)[ \t]+|[ \t]+)#[0-9]+)*)`)
	legacyRef  = regexp.MustCompile(`#([0-9]+)`)
	// dependsOnLine is the canonical marker: "depends_on:" at the start of a line, optionally
	// inside quotes and after a list marker.
	dependsOnLine = regexp.MustCompile(`^[ \t]*(?:>[ \t]*)*(?:[-*+][ \t]*)?depends_on:(.*)$`)
	dependsOnRef  = regexp.MustCompile(`^#([0-9]+)$`)
	// fenceLine opens or closes a fenced code block, also inside quotes.
	fenceLine = regexp.MustCompile("^[ \\t]*(?:>[ \\t]*)*(`{3,}|~{3,})(.*)$")
)

// maxQuotedLine caps how much of a malformed line the error echoes: the line comes from the
// issue author and ends up in the needs-info comment.
const maxQuotedLine = 80

// Dependencies lists the issue numbers a body declares as blockers, sorted and unique. Lines
// "depends_on: #N" win: when the body has any, the legacy mentions ("blocked by #N") are ignored.
// The first malformed "depends_on:" line is an error that quotes it. Lines inside fenced code
// blocks are examples, not markers, for both forms; a legacy mention counts only at the start
// of a line, so a sentence that merely talks about an issue declares nothing.
func Dependencies(body string) ([]int, error) {
	var out, legacy []int
	marked := false
	fence := "" // the open fence, "" outside a code block
	for line := range strings.SplitSeq(strings.ReplaceAll(body, "\r", ""), "\n") {
		if f := fenceLine.FindStringSubmatch(line); f != nil {
			switch {
			case fence == "":
				fence = f[1]
				continue
			case f[1][0] == fence[0] && len(f[1]) >= len(fence) && strings.TrimSpace(f[2]) == "":
				fence = ""
				continue
			}
		}
		if fence != "" {
			continue
		}
		m := dependsOnLine.FindStringSubmatch(line)
		if m == nil {
			if l := legacyLine.FindStringSubmatch(line); l != nil {
				for _, r := range legacyRef.FindAllStringSubmatch(l[1], -1) {
					if n, err := strconv.Atoi(r[1]); err == nil && !slices.Contains(legacy, n) {
						legacy = append(legacy, n)
					}
				}
			}
			continue
		}
		marked = true
		refs := strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		if len(refs) == 0 {
			return nil, malformedDependsOn(line)
		}
		for _, ref := range refs {
			d := dependsOnRef.FindStringSubmatch(ref)
			if d == nil {
				return nil, malformedDependsOn(line)
			}
			n, err := strconv.Atoi(d[1])
			if err != nil || n <= 0 {
				return nil, malformedDependsOn(line)
			}
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	if !marked {
		out = legacy
	}
	slices.Sort(out)
	return out, nil
}

func malformedDependsOn(line string) error {
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > maxQuotedLine {
		line = string(r[:maxQuotedLine]) + "…"
	}
	return fmt.Errorf("linha %q: esperado #N separados por vírgula ou espaço", line)
}
