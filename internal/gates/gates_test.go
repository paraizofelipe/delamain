package gates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/artifacts"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetectShouldPickGoCommands(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"go.mod": "module x\n"})
	s := Detect(dir)
	if s.Name != "go" || s.Commands.Test != "go test ./..." || s.Commands.Lint != "go vet ./..." || !strings.Contains(s.Commands.FormatCheck, "gofmt -l") {
		t.Fatalf("go stack = %+v", s)
	}
}

func TestDetectShouldUseNodeScriptsWithTheLockfilePackageManager(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"package.json":   `{"scripts":{"test":"vitest run","lint":"eslint .","typecheck":"tsc --noEmit","format:check":"prettier -c ."}}`,
		"pnpm-lock.yaml": "",
	})
	c := Detect(dir).Commands
	want := artifacts.Commands{Test: "pnpm run test", Lint: "pnpm run lint", Typecheck: "pnpm run typecheck", FormatCheck: "pnpm run format:check"}
	if c != want {
		t.Fatalf("node commands = %+v, want %+v", c, want)
	}
}

func TestDetectShouldIgnoreNpmPlaceholderTestScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`})
	if c := Detect(dir).Commands; c.Test != "" {
		t.Fatalf("npm's placeholder is not a test suite: %+v", c)
	}
}

func TestDetectShouldUsePytestAndRuffForPython(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"pyproject.toml": "[project]\nname='x'\n[tool.ruff]\n", "uv.lock": ""})
	c := Detect(dir).Commands
	if c.Test != "uv run pytest -q" || c.Lint != "uv run ruff check ." || c.FormatCheck != "uv run ruff format --check ." || c.Typecheck != "" {
		t.Fatalf("python commands = %+v", c)
	}
}

func TestDetectShouldReturnUnknownForOtherRepos(t *testing.T) {
	if s := Detect(t.TempDir()); s.Name != "" || s.Commands != (artifacts.Commands{}) {
		t.Fatalf("stack = %+v", s)
	}
}

func TestEffectiveShouldPreferRepoConfigThenDetectionThenPlan(t *testing.T) {
	repo := artifacts.Commands{Test: "make test"}
	detected := artifacts.Commands{Test: "go test ./...", Lint: "go vet ./..."}
	plan := artifacts.Commands{Test: "true", Lint: "golangci-lint run", Typecheck: "staticcheck ./..."}
	got, sources := Effective(repo, detected, plan)
	want := artifacts.Commands{Test: "make test", Lint: "go vet ./...", Typecheck: "staticcheck ./..."}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if sources["test"] != "repo" || sources["lint"] != "detectado" || sources["typecheck"] != "planner" {
		t.Fatalf("sources = %v", sources)
	}
}

func TestLoadRepoConfigShouldReadCommandsAndGates(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{".delamain.yaml": "commands:\n  test: make test\ngates:\n  red_proof: always\n  require_tests: false\n"})
	rc, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Commands.Test != "make test" || rc.Gates.RedProof != "always" || rc.Gates.RequireTests == nil || *rc.Gates.RequireTests {
		t.Fatalf("repo config = %+v", rc)
	}
}

func TestLoadRepoConfigShouldTolerateMissingFileAndRejectTypos(t *testing.T) {
	if rc, err := LoadRepoConfig(t.TempDir()); err != nil || rc.Commands.Test != "" {
		t.Fatalf("missing file is fine: %+v %v", rc, err)
	}
	dir := t.TempDir()
	write(t, dir, map[string]string{".delamain.yaml": "comands:\n  test: x\n"})
	if _, err := LoadRepoConfig(dir); err == nil {
		t.Fatal("an unknown key must fail instead of silently using defaults")
	}
	write(t, dir, map[string]string{".delamain.yaml": "gates:\n  red_proof: sometimes\n"})
	if _, err := LoadRepoConfig(dir); err == nil {
		t.Fatal("unknown red_proof policy must fail")
	}
}

func TestIsTestShouldRecognizeCommonConventions(t *testing.T) {
	tests := []string{"calc_test.go", "src/a.test.ts", "src/a.spec.tsx", "web/__tests__/x.js", "tests/test_api.py", "pkg/api_test.py", "test/unit/a.rb", "spec/models/user_spec.rb"}
	prod := []string{"calc.go", "src/a.ts", "pkg/api.py", "testdata.go", "contest.go", "attest/a.go"}
	for _, f := range tests {
		if !IsTest(f) {
			t.Errorf("%s is a test file", f)
		}
	}
	for _, f := range prod {
		if IsTest(f) {
			t.Errorf("%s is not a test file", f)
		}
	}
}

func TestTestsTouchedShouldFailWhenProductionChangesWithoutTests(t *testing.T) {
	if ok, detail := TestsTouched([]string{"calc.go", "README.md"}); ok || !strings.Contains(detail, "calc.go") {
		t.Fatalf("prod change without test must fail: %v %q", ok, detail)
	}
	if ok, _ := TestsTouched([]string{"calc.go", "calc_test.go"}); !ok {
		t.Fatal("prod + test is fine")
	}
	if ok, _ := TestsTouched([]string{"README.md", "docs/x.md", ".gitlab-ci.yml"}); !ok {
		t.Fatal("docs/config-only changes need no tests")
	}
	if ok, _ := TestsTouched(nil); !ok {
		t.Fatal("no change, nothing to prove (tem-commits covers empty work)")
	}
}

func TestMatrixShouldAcceptReferencesThatExistInCode(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"calc_test.go":  "func TestAdd(t *testing.T) { t.Run(\"should sum when positive\", nil) }",
		"web/a.test.ts": "it('shows total', () => {})",
	})
	plan := artifacts.Plan{
		Kind:            "bug",
		Acceptance:      []artifacts.Acceptance{{Criterion: "soma", Tests: []string{"calc_test.go::TestAdd/should sum when positive", "web/a.test.ts::shows total"}}},
		RegressionTests: []string{"calc_test.go::TestAdd"},
	}
	if problems := Matrix(dir, plan, []string{"calc_test.go"}); len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestMatrixShouldReportMissingFilesNamesAndUntouchedRegressionTests(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"calc_test.go": "func TestAdd(t *testing.T) {}"})
	plan := artifacts.Plan{
		Kind: "bug",
		Acceptance: []artifacts.Acceptance{
			{Criterion: "arquivo inexistente", Tests: []string{"nope_test.go::TestX"}},
			{Criterion: "nome inexistente", Tests: []string{"calc_test.go::TestSub"}},
		},
		RegressionTests: []string{"calc_test.go::TestAdd"},
	}
	problems := strings.Join(Matrix(dir, plan, nil), "\n")
	for _, want := range []string{"nope_test.go", "TestSub", "regressão"} {
		if !strings.Contains(problems, want) {
			t.Errorf("missing %q in:\n%s", want, problems)
		}
	}
}

func TestMatrixShouldRejectPathsEscapingTheWorktree(t *testing.T) {
	plan := artifacts.Plan{Acceptance: []artifacts.Acceptance{{Criterion: "c", Tests: []string{"../../etc/passwd::root"}}}}
	if problems := Matrix(t.TempDir(), plan, nil); len(problems) != 1 {
		t.Fatalf("an agent-supplied path must stay inside the worktree: %v", problems)
	}
}

func TestDependenciesShouldFindBlockingIssueReferences(t *testing.T) {
	body := "Blocked by #12.\nDepende de #3 e depends on #40\nbloqueada por #7\nveja #99"
	got := Dependencies(body)
	want := []int{3, 7, 12, 40}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseRepoConfigShouldTreatEmptyInputAsEmptyConfigAndRejectTypos(t *testing.T) {
	for name, raw := range map[string]string{"nil": "", "blank": "\n  \n", "comment only": "# nada\n"} {
		if rc, err := ParseRepoConfig([]byte(raw)); err != nil || rc.Commands.Test != "" {
			t.Errorf("%s: an empty file is an empty config: %+v %v", name, rc, err)
		}
	}
	rc, err := ParseRepoConfig([]byte("commands:\n  test: make test\ngates:\n  red_proof: never\n"))
	if err != nil || rc.Commands.Test != "make test" || rc.Gates.RedProof != "never" {
		t.Fatalf("repo config = %+v %v", rc, err)
	}
	for name, raw := range map[string]string{
		"typo":           "comands:\n  test: x\n",
		"unknown policy": "gates:\n  red_proof: sometimes\n",
		"not a mapping":  "- test\n",
	} {
		if _, err := ParseRepoConfig([]byte(raw)); err == nil || !strings.Contains(err.Error(), RepoConfigFile) {
			t.Errorf("%s: must fail naming %s, got %v", name, RepoConfigFile, err)
		}
	}
}

func TestParseRepoConfigShouldKeepReadingOnlyTheFirstDocumentLikeLoadRepoConfig(t *testing.T) {
	raw := "commands:\n  test: make test\n---\nnotes: anything after the first document is not config\n"
	rc, err := ParseRepoConfig([]byte(raw))
	if err != nil || rc.Commands.Test != "make test" {
		t.Fatalf("a repository whose .delamain.yaml has more documents keeps working: %+v %v", rc, err)
	}
	dir := t.TempDir()
	write(t, dir, map[string]string{".delamain.yaml": raw})
	if rc, err := LoadRepoConfig(dir); err != nil || rc.Commands.Test != "make test" {
		t.Fatalf("issue runs read the same config as before: %+v %v", rc, err)
	}
}
