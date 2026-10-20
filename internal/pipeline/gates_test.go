package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
)

func checkNamed(it Iteration, name string) (CheckResult, bool) {
	for _, c := range it.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckResult{}, false
}

func TestVerifyShouldRunEveryGateOnTheHappyPath(t *testing.T) {
	hs := newHarness(t, happy)
	res := hs.run(t)
	if res.Outcome != OutcomeApproved {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
	var names []string
	for _, c := range res.History[0].Checks {
		names = append(names, c.Name)
	}
	want := []string{"worktree-limpa", "tem-commits", "commits", "testes-tocados", "matriz-criterios", "lint", "test", "prova-vermelho", "segredos"}
	if !slices.Equal(names, want) {
		t.Fatalf("gates = %v, want %v", names, want)
	}
	if !slices.Equal(hs.g.redproof, []string{"add main", "checkout fix/7-fix-add calc_test.go", "remove"}) {
		t.Fatalf("red proof must apply only the branch tests onto base and clean up: %v", hs.g.redproof)
	}
	if sec, _ := checkNamed(res.History[0], "segredos"); !sec.Skipped {
		t.Fatal("without gitleaks the secrets gate is reported as skipped, never silently dropped")
	}
}

func TestRedProofShouldFailWhenNewTestsAlreadyPassOnBase(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.checks = func(string, int) int { return 0 } // tautological test: green even without the fix
	res := hs.run(t)

	if res.Outcome != OutcomeExhausted {
		t.Fatalf("a test that does not catch the bug must never be approved, got %s", res.Outcome)
	}
	rp, ok := checkNamed(res.History[0], "prova-vermelho")
	if !ok || rp.Passed() || !strings.Contains(rp.Tail, "passam no base") {
		t.Fatalf("red proof = %+v", rp)
	}
	if !strings.Contains(hs.h.prompts["implementer"][1], "prova-vermelho") {
		t.Fatal("the implementer must be told why the iteration was rejected")
	}
	if len(hs.h.prompts["reviewer"]) != 0 {
		t.Fatal("review is pointless while a deterministic gate fails")
	}
}

func TestRedProofShouldBeSkippedForFeaturesByDefault(t *testing.T) {
	hs := newHarness(t, happy)
	hs.task.Kind = "feature"
	hs.task.Title = "Add sum"
	planFeature := strings.Replace(planJSON, `"kind":"bug"`, `"kind":"feature"`, 1)
	hs.h.behave = func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: planFeature}
		}
		return happy(role, n, text)
	}
	res := hs.run(t)
	if _, ok := checkNamed(res.History[0], "prova-vermelho"); ok || res.Outcome != OutcomeApproved {
		t.Fatalf("red_proof=bug skips features: %s %v", res.Outcome, res.History[0].Checks)
	}
}

func TestRedProofShouldApplyWhenPlannerClassifiesAsBug(t *testing.T) {
	hs := newHarness(t, happy)
	hs.task.Kind = "feature" // the issue looked like a feature, the planner found a bug
	res := hs.run(t)
	if _, ok := checkNamed(res.History[0], "prova-vermelho"); !ok {
		t.Fatalf("an agent cannot opt out of the red proof by reclassifying: %v", res.History[0].Checks)
	}
}

func TestTestsTouchedShouldRejectProductionChangeWithoutTests(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "implementer" {
			return reply{write: implJSON, mutate: func(g *fakeGit) {
				g.commitFiles("fix: add", map[string]string{"calc.go": "fixed"})
			}}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	tt, _ := checkNamed(res.History[0], "testes-tocados")
	if res.Outcome != OutcomeExhausted || tt.Passed() || !strings.Contains(tt.Tail, "calc.go") {
		t.Fatalf("got %s, gate %+v", res.Outcome, tt)
	}
}

func TestRepoConfigShouldOverridePlannerCommandsAndGates(t *testing.T) {
	hs := newHarness(t, happy)
	hs.g.onWorktree = func(path string) {
		_ = os.WriteFile(filepath.Join(path, ".delamain.yaml"), []byte("commands:\n  test: make test\ngates:\n  red_proof: never\n"), 0o600)
	}
	res := hs.run(t)
	test, _ := checkNamed(res.History[0], "test")
	if test.Command != "make test" || res.CommandSources["test"] != "repo" {
		t.Fatalf("repo config must win over the planner: %+v %v", test, res.CommandSources)
	}
	if _, ok := checkNamed(res.History[0], "prova-vermelho"); ok {
		t.Fatal("red_proof: never in the repo disables the gate")
	}
}

func TestDetectedStackShouldBeatPlannerCommands(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: strings.Replace(planJSON, `"test":"go test ./..."`, `"test":"true"`, 1)}
		}
		return happy(role, n, text)
	})
	hs.g.onWorktree = func(path string) { _ = os.WriteFile(filepath.Join(path, "go.mod"), []byte("module x\n"), 0o600) }
	res := hs.run(t)
	test, _ := checkNamed(res.History[0], "test")
	if test.Command != "go test ./..." {
		t.Fatalf("an agent must not be able to replace the test suite with `true`: %+v", test)
	}
	if fmtc, ok := checkNamed(res.History[0], "format"); !ok || !strings.Contains(fmtc.Command, "gofmt") {
		t.Fatalf("detection fills gates the planner left empty: %+v", res.History[0].Checks)
	}
}

// withImplementerFiles wraps happy so the implementer also commits files (e.g. a new go.mod).
func withImplementerFiles(files map[string]string, extra func(g *fakeGit)) behavior {
	return func(role string, n int, text string) reply {
		rep := happy(role, n, text)
		if role == "implementer" {
			orig := rep.mutate
			rep.mutate = func(g *fakeGit) {
				if orig != nil {
					orig(g)
				}
				if extra != nil {
					extra(g)
				}
				g.commitFiles("chore: add files", files)
			}
		}
		return rep
	}
}

func TestStackCreatedByTheImplementerShouldAddItsGatesWhenTheBaseHadNone(t *testing.T) {
	hs := newHarness(t, withImplementerFiles(map[string]string{"go.mod": "module x\n"}, nil))
	res := hs.run(t)
	lint, ok := checkNamed(res.History[0], "lint")
	if !ok || lint.Command != "go vet ./..." || res.CommandSources["lint"] != "detectado" {
		t.Fatalf("a brand-new Go module must still get vet/gofmt, not only the planner's test: %+v %v", res.History[0].Checks, res.CommandSources)
	}
	if _, ok := checkNamed(res.History[0], "format"); !ok {
		t.Fatalf("format gate missing: %+v", res.History[0].Checks)
	}
}

func TestStackShouldNotBeRedetectedWhenTheBaseHadOne(t *testing.T) {
	hs := newHarness(t, withImplementerFiles(
		map[string]string{"package.json": `{"scripts":{"test":"true"}}`},
		func(g *fakeGit) { _ = os.Remove(filepath.Join(g.wt, "go.mod")) },
	))
	hs.g.onWorktree = func(path string) { _ = os.WriteFile(filepath.Join(path, "go.mod"), []byte("module x\n"), 0o600) }
	res := hs.run(t)
	if test, _ := checkNamed(res.History[0], "test"); test.Command != "go test ./..." {
		t.Fatalf("an agent must not swap the base stack's gates for its own: %+v", test)
	}
}

func TestInvalidRepoConfigShouldHaltBeforeAgentsWork(t *testing.T) {
	hs := newHarness(t, happy)
	hs.g.onWorktree = func(path string) {
		_ = os.WriteFile(filepath.Join(path, ".delamain.yaml"), []byte("comands: {}\n"), 0o600)
	}
	res := hs.run(t)
	if res.Outcome != OutcomeHalted || !strings.Contains(res.Reason, ".delamain.yaml") || len(hs.h.prompts["planner"]) != 0 {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestMatrixShouldRejectCriteriaPointingToMissingTests(t *testing.T) {
	hs := newHarness(t, func(role string, n int, text string) reply {
		if role == "planner" {
			return reply{write: strings.ReplaceAll(planJSON, "calc_test.go::TestAdd", "calc_test.go::TestAddNegative")}
		}
		return happy(role, n, text)
	})
	res := hs.run(t)
	m, _ := checkNamed(res.History[0], "matriz-criterios")
	if m.Passed() || !strings.Contains(m.Tail, "TestAddNegative") {
		t.Fatalf("matrix gate = %+v", m)
	}
}

func TestSecretsGateShouldRunGitleaksOnTheBranchCommitsWhenInstalled(t *testing.T) {
	hs := newHarness(t, happy)
	hs.r.LookPath = func(string) (string, error) { return "/usr/bin/gitleaks", nil }
	hs.h.checks = func(name string, n int) int {
		if name == "secrets" {
			return 1
		}
		return defaultChecks(name, n)
	}
	res := hs.run(t)
	sec, _ := checkNamed(res.History[0], "segredos")
	if sec.Skipped || sec.Passed() || !strings.Contains(sec.Command, "--log-opts=main..HEAD") || res.Outcome != OutcomeExhausted {
		t.Fatalf("a leaked secret must block: %s %+v", res.Outcome, sec)
	}
}

func TestPreflightShouldRejectTasksBlockedByOpenIssues(t *testing.T) {
	hs := newHarness(t, happy)
	f := withForge(hs)
	f.issues = map[int]forge.Issue{3: {Number: 3, Open: true}, 4: {Number: 4, Open: false}}
	hs.task.Body += "\n\nDepende de #3 e blocked by #4"
	res := hs.run(t)
	if res.Outcome != OutcomeNeedsInfo || !strings.Contains(res.Reason, "#3") || strings.Contains(res.Reason, "#4") {
		t.Fatalf("only the open blocker counts: %s %s", res.Outcome, res.Reason)
	}
	if runs, _ := hs.st.UnfinishedRuns(context.Background()); len(runs) != 0 {
		t.Fatal("blocked tasks create no run")
	}
}

func TestPreflightShouldFailClosedWhenABlockerCannotBeChecked(t *testing.T) {
	hs := newHarness(t, happy)
	withForge(hs) // no issue #9 known: lookup fails
	hs.task.Body += "\n\ndepends on #9"
	if res := hs.run(t); res.Outcome != OutcomeNeedsInfo || !strings.Contains(res.Reason, "#9") {
		t.Fatalf("got %s: %s", res.Outcome, res.Reason)
	}
}

func TestVerifyShouldFailWhenACommitSubjectIsTooLong(t *testing.T) {
	hs := newHarness(t, happy)
	hs.h.behave = func(role string, n int, text string) reply {
		if role == "implementer" && n == 1 {
			return reply{write: implJSON, mutate: func(g *fakeGit) {
				g.commitFiles("fix: "+strings.Repeat("a", 80), map[string]string{
					"calc.go":      "func Add(a, b int) int { return a + b }",
					"calc_test.go": "func TestAdd(t *testing.T) {}",
				})
			}}
		}
		return happy(role, n, text)
	}
	res := hs.run(t)

	// A rule can measure this, so it must never cost a review iteration.
	c, ok := checkNamed(res.History[0], "commits")
	if !ok || c.Passed() {
		t.Fatalf("commits gate = %+v, want a failure", c)
	}
	if !strings.Contains(c.Tail, "limite") {
		t.Fatalf("the detail must name the limit it broke: %q", c.Tail)
	}
	if len(hs.h.prompts["reviewer"]) != 0 {
		t.Fatal("a commit subject must be fixed before the reviewer is paid to read the diff")
	}
	if !strings.Contains(hs.h.prompts["implementer"][1], "commits") {
		t.Fatal("the implementer must be told which gate rejected the iteration")
	}
}
