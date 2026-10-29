package gates

import (
	"os"
	"path/filepath"
	"slices"
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

func TestDependenciesShouldKeepTheLegacyRegexWithoutDependsOn(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"legacy mentions":           {"Blocked by #12.\nDepende de #3 e depends on #40\nbloqueada por #7\nveja #99", []int{3, 7, 12}},
		"marker in the middle":      {"veja depends_on: #5 no meio da frase\nblocked by #2", []int{2}},
		"marker with another case":  {"Depends_On: #5\nblocked by #2", []int{2}},
		"marker with spaced colon":  {"depends_on : #5\nblocked by #2", []int{2}},
		"marker as a checkbox item": {"- [ ] depends_on: #5\nblocked by #2", []int{2}},
		"empty body":                {"", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestLegacyDependenciesShouldIgnoreDeclarationsInTheMiddleOfASentence(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"negated sentence":       {"isso não depende de #82", nil},
		"issue depends on issue": {"a #45 depende de #46", nil},
		"after a colon":          {"veja: blocked by #3", nil},
		"leading spaces":         {"   blocked by #4", []int{4}},
		"mixed body":             {"blocked by #1\nisso não depende de #2\nDepends on #3", []int{1, 3}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestLegacyDependenciesShouldIgnoreCodeBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"backticks":            {"```\nblocked by #5\n```\nblocked by #6", []int{6}},
		"tildes":               {"~~~text\ndepende de #5\n~~~", nil},
		"fence inside a quote": {"> ```\n> blocked by #5\n> ```\nblocked by #6", []int{6}},
		"unclosed fence":       {"blocked by #6\n```\nblocked by #5", []int{6}},
		"after the closing":    {"```go\ndepends on #5\n```\ndepends on #6", []int{6}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestLegacyDependenciesShouldAcceptQuotesListsAndCheckboxes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"quoted item":        {"> - depende de #72", []int{72}},
		"open checkbox":      {"- [ ] blocked by #9", []int{9}},
		"nested quote":       {">> depends on #5", []int{5}},
		"checked star item":  {"* [x] bloqueado por #6", []int{6}},
		"plus item":          {"+ bloqueada por #7", []int{7}},
		"ordered item":       {"1. depende de #8", []int{8}},
		"indented, any case": {"  - [X] Blocked By #10", []int{10}},
		"unknown checkbox":   {"[y] blocked by #9", nil},
		"double dash":        {"-- blocked by #9", nil},
		"dash without space": {"-blocked by #9", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestLegacyDependenciesShouldReadListsOfIssues(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"commas and e":       {"depende de #82, #83 e #84", []int{82, 83, 84}},
		"and":                {"blocked by #82 and #83", []int{82, 83}},
		"spaces":             {"depends on #82 #83", []int{82, 83}},
		"e":                  {"depende de #82 e #83", []int{82, 83}},
		"no spaces":          {"blocked by #82,#83", []int{82, 83}},
		"stops at a word":    {"depende de #82 para liberar #83", []int{82}},
		"trailing dot":       {"blocked by #82 and #83.", []int{82, 83}},
		"duplicates, sorted": {"blocked by #83, #82 E #83\n- depende de #82", []int{82, 83}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestLegacyDependenciesShouldAcceptDaDasDoDos(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"da":             {"depende da #82", []int{82}},
		"das":            {"Depende das #1 e #2", []int{1, 2}},
		"do":             {"depende do #3", []int{3}},
		"dos":            {"depende dos #4, #5", []int{4, 5}},
		"other word":     {"depende di #6", nil},
		"glued word":     {"depende dados #6", nil},
		"no preposition": {"depende #6", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestDependenciesShouldReadDependsOnLines(t *testing.T) {
	for name, body := range map[string]string{
		"dash item":       "Contexto.\n\n- depends_on: #81\n",
		"no marker":       "depends_on: #81",
		"quote":           "> depends_on: #81",
		"quoted item":     "> - depends_on: #81",
		"star item":       "* depends_on: #81",
		"plus item":       "+ depends_on: #81",
		"indented":        "  \t- depends_on: #81",
		"nested quote":    ">> depends_on: #81",
		"crlf body":       "Contexto.\r\n\r\n- depends_on: #81\r\n",
		"trailing spaces": "- depends_on:   #81   ",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(body)
			if err != nil || !slices.Equal(got, []int{81}) {
				t.Fatalf("got %v %v, want [81]", got, err)
			}
		})
	}
	got, err := Dependencies("- depends_on: #82\n- depends_on: #81")
	if err != nil || !slices.Equal(got, []int{81, 82}) {
		t.Fatalf("two lines: got %v %v, want [81 82]", got, err)
	}
}

func TestDependenciesShouldReadSeveralIssuesInOneDependsOnLine(t *testing.T) {
	for name, line := range map[string]string{
		"commas":           "- depends_on: #82, #83",
		"spaces":           "- depends_on: #82 #83",
		"no spaces, dup":   "- depends_on: #83,#82,#82",
		"loose separators": "- depends_on: #82 , #83,",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(line)
			if err != nil || !slices.Equal(got, []int{82, 83}) {
				t.Fatalf("got %v %v, want [82 83]", got, err)
			}
		})
	}
}

func TestDependenciesShouldIgnoreTheLegacyRegexWhenDependsOnIsPresent(t *testing.T) {
	got, err := Dependencies("blocked by #4\nDepende de #5\n- depends_on: #3")
	if err != nil || !slices.Equal(got, []int{3}) {
		t.Fatalf("got %v %v, want [3]", got, err)
	}
}

func TestDependenciesShouldRejectMalformedDependsOnLines(t *testing.T) {
	for name, tc := range map[string]struct{ body, line string }{
		"number without hash":    {"- depends_on: 81", "- depends_on: 81"},
		"trailing words":         {"depends_on: #81 até sair", "depends_on: #81 até sair"},
		"zero":                   {"depends_on: #0", "depends_on: #0"},
		"empty":                  {"depends_on:", "depends_on:"},
		"blank":                  {"- depends_on:   ", "- depends_on:"},
		"bare hash":              {"depends_on: #", "depends_on: #"},
		"negative":               {"depends_on: #-1", "depends_on: #-1"},
		"signed":                 {"depends_on: #+1", "depends_on: #+1"},
		"letters":                {"depends_on: #8a", "depends_on: #8a"},
		"trailing dot":           {"depends_on: #81.", "depends_on: #81."},
		"overflow":               {"depends_on: #99999999999999999999", "depends_on: #99999999999999999999"},
		"valid then malformed":   {"- depends_on: #3\nblocked by #4\n- depends_on: 81", "- depends_on: 81"},
		"first malformed counts": {"depends_on: x\ndepends_on: y", "depends_on: x"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.line) {
				t.Fatalf("got %v %v, want an error quoting %q", got, err, tc.line)
			}
		})
	}
	if got, err := Dependencies("depends_on: #081"); err != nil || !slices.Equal(got, []int{81}) {
		t.Fatalf("leading zeros are still a number: %v %v", got, err)
	}
}

func TestDependenciesShouldQuoteAndTruncateTheMalformedLineInTheError(t *testing.T) {
	_, err := Dependencies("depends_on: #1 " + strings.Repeat("x", 10000) + "\x1b")
	if err == nil || len(err.Error()) >= 200 || !strings.Contains(err.Error(), `…"`) {
		t.Fatalf("a long line is cut short: %v", err)
	}
	_, err = Dependencies("depends_on: #1 \x1b[31mvermelho")
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `\x1b[31mvermelho"`) {
		t.Fatalf("control characters are escaped: %q", err)
	}
}

func TestDependenciesShouldIgnoreDependsOnInsideCodeBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []int
	}{
		"backticks":            {"```\n- depends_on: #5\n```\n- depends_on: #6", []int{6}},
		"tildes":               {"~~~yaml\n- depends_on: #5\n~~~\n- depends_on: #6", []int{6}},
		"malformed inside":     {"```\ndepends_on: 81\n```\n- depends_on: #6", []int{6}},
		"long fence":           {"````\n```\n- depends_on: #5\n````\n- depends_on: #6", []int{6}},
		"other fence char":     {"```\n~~~\n- depends_on: #5\n```\n- depends_on: #6", []int{6}},
		"unclosed fence":       {"- depends_on: #6\n```\n- depends_on: #5\ndepends_on: 81", []int{6}},
		"fence inside a quote": {"> ```\n> - depends_on: #5\n> ```\n- depends_on: #6", []int{6}},
		"crlf fence":           {"```\r\n- depends_on: #5\r\n```\r\n- depends_on: #6\r\n", []int{6}},
		"only inside a block":  {"```\n- depends_on: #5\n```\nblocked by #7", []int{7}},
		"indented code counts": {"    - depends_on: #6\nblocked by #7", []int{6}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Dependencies(tc.body)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, err, tc.want)
			}
		})
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

func TestCommitsShouldFailWhenASubjectExceedsTheLimit(t *testing.T) {
	long := strings.Repeat("a", 71)
	ok, detail := Commits([]string{"abc1234 fix: short enough", "def5678 fix: " + long}, 0)
	if ok {
		t.Fatal("a subject past the limit must fail the gate, not reach the reviewer")
	}
	if !strings.Contains(detail, "def5678") || !strings.Contains(detail, "limite 72") {
		t.Fatalf("detail must name the commit and the limit it broke: %q", detail)
	}
	if strings.Contains(detail, "abc1234") {
		t.Fatalf("a compliant subject must not be reported: %q", detail)
	}
}

func TestCommitsShouldCountCharactersNotBytes(t *testing.T) {
	// 72 accented characters are 144 bytes: counting bytes would reject a valid pt-BR subject.
	ok, detail := Commits([]string{"abc1234 " + strings.Repeat("ã", 72)}, 0)
	if !ok {
		t.Fatalf("a 72-character subject is within the limit: %q", detail)
	}
	if ok, _ := Commits([]string{"abc1234 " + strings.Repeat("ã", 73)}, 0); ok {
		t.Fatal("73 characters must still break the 72-character limit")
	}
}

func TestCommitsShouldUseTheRepoConfiguredLimitOverTheDefault(t *testing.T) {
	subject := strings.Repeat("a", 70) // fine by default, too long for a repo that documents 69
	if ok, _ := Commits([]string{"abc1234 " + subject}, 0); !ok {
		t.Fatal("70 characters pass the default limit of 72")
	}
	ok, detail := Commits([]string{"abc1234 " + subject}, 69)
	if ok || !strings.Contains(detail, "limite 69") {
		t.Fatalf("the repository's stricter limit must win: ok=%v detail=%q", ok, detail)
	}
}

func TestPRTitleShouldPassConventionalEnglishTitle(t *testing.T) {
	if ok, detail := PRTitle("feat: add a pr title to the impl artifact", 0); !ok {
		t.Fatalf("a commitlint title in English within the limit must pass: %q", detail)
	}
}

func TestPRTitleShouldPassScopedAndBreakingTitles(t *testing.T) {
	for _, title := range []string{
		"fix(pipeline): inject the plan refs",
		"feat!: drop the legacy dependency reader",
		"refactor(gates.v2)!: split the commit gate",
	} {
		if ok, detail := PRTitle(title, 0); !ok {
			t.Fatalf("scope and breaking marker are valid commitlint: %q -> %q", title, detail)
		}
	}
}

func TestPRTitleShouldFailWithoutConventionalType(t *testing.T) {
	for name, title := range map[string]string{
		"unknown type":         "pipeline: inject refs",
		"capitalized type":     "Feature: x",
		"no space after colon": "feat:x",
	} {
		t.Run(name, func(t *testing.T) {
			ok, detail := PRTitle(title, 0)
			if ok {
				t.Fatalf("%q is not Conventional Commits and must fail", title)
			}
			if !strings.Contains(detail, "fora do formato Conventional Commits") || !strings.Contains(detail, title) {
				t.Fatalf("detail must name the format rule and the title: %q", detail)
			}
		})
	}
}

func TestLoadRepoConfigShouldReadTheSubjectLimit(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{RepoConfigFile: "gates:\n  subject_limit: 69\n"})
	rc, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Gates.SubjectLimit != 69 {
		t.Fatalf("SubjectLimit = %d, want 69", rc.Gates.SubjectLimit)
	}
}

func TestParseRepoConfigShouldReadTheAgentOverrides(t *testing.T) {
	rc, err := ParseRepoConfig([]byte("agent:\n  harness: claude\nclaude:\n  binary: mia\n  args: [launch, claude]\nomp:\n  model: openai/gpt-x\nagents:\n  implementer:\n    model: haiku\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(rc.Agent.Harness) != "claude" || rc.Claude.Binary != "mia" || len(rc.Claude.Args) != 2 {
		t.Fatalf("agent and claude overrides: %+v", rc)
	}
	if rc.Omp.Model != "openai/gpt-x" || rc.Agents["implementer"].Model != "haiku" {
		t.Fatalf("omp and agents overrides: %+v", rc)
	}
	if _, err := ParseRepoConfig([]byte("claud:\n  binary: mia\n")); err == nil {
		t.Fatal("unknown keys must still be rejected")
	}
}

func TestPRTitleShouldFailWhenEmpty(t *testing.T) {
	for _, title := range []string{"", "   "} {
		ok, detail := PRTitle(title, 0)
		if ok {
			t.Fatalf("an empty title %q must fail: the PR needs a title", title)
		}
		if detail != "título do PR vazio" {
			t.Fatalf("an empty title fails alone, with the empty reason only: %q", detail)
		}
	}
}

func TestPRTitleShouldFailWhenLongerThanLimit(t *testing.T) {
	// "feat: " is 6 characters, so 66 more make exactly 72.
	if ok, detail := PRTitle("feat: "+strings.Repeat("a", 66), 0); !ok {
		t.Fatalf("a 72-character title is within the default limit: %q", detail)
	}
	// 66 em dashes are 198 bytes: counting bytes would reject a 72-character title.
	if ok, detail := PRTitle("feat: "+strings.Repeat("—", 66), 0); !ok {
		t.Fatalf("the limit counts characters, not bytes: %q", detail)
	}
	ok, detail := PRTitle("feat: "+strings.Repeat("a", 67), 0)
	if ok || !strings.Contains(detail, "título do PR com 73 caracteres (limite 72)") {
		t.Fatalf("73 characters must break the default limit of 72: ok=%v detail=%q", ok, detail)
	}
	ok, detail = PRTitle("feat: "+strings.Repeat("a", 64), 69)
	if ok || !strings.Contains(detail, "título do PR com 70 caracteres (limite 69)") {
		t.Fatalf("an explicit limit must win over the default: ok=%v detail=%q", ok, detail)
	}
}

func TestPRTitleShouldFailWhenDescriptionStartsUppercase(t *testing.T) {
	for _, title := range []string{"feat: Add a pr title", "fix(gates)!: Reject long titles"} {
		ok, detail := PRTitle(title, 0)
		if ok || !strings.Contains(detail, "título do PR com descrição em maiúscula: "+title) {
			t.Fatalf("commitlint wants a lowercase description: %q -> ok=%v detail=%q", title, ok, detail)
		}
	}
	if ok, detail := PRTitle("feat: add PR title to impl", 0); !ok {
		t.Fatalf("only the first letter of the description matters, not acronyms later on: %q", detail)
	}
}

func TestPRTitleShouldFailWithTrailingPeriod(t *testing.T) {
	title := "feat: add a pr title to the impl artifact."
	ok, detail := PRTitle(title, 0)
	if ok || !strings.Contains(detail, "título do PR termina com ponto: "+title) {
		t.Fatalf("commitlint forbids a trailing period: ok=%v detail=%q", ok, detail)
	}
	if ok, detail := PRTitle("fix: read v1.2 configs", 0); !ok {
		t.Fatalf("a period inside the description is fine: %q", detail)
	}
}
