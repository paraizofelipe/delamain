package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSlugShouldProduceAsciiKebabCaseWithinLimit(t *testing.T) {
	cases := map[string]string{
		"Corrigir validação de e-mail!":         "corrigir-validacao-de",
		"  Add   OAuth2 login (GitHub)  ":       "add-oauth2-login",
		"!!!":                                   "task",
		"#123":                                  "123",
		"a very long title that keeps going on": "a-very-long-title-that",
		"supercalifragilisticexpialidocious":    "supercalifragilisticex", // single word: hard cut
	}
	for in, want := range cases {
		if got := Slug(in, 22); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBranchNameShouldUseFixForBugsAndFeatOtherwise(t *testing.T) {
	if got := BranchName("bug", "#12", "Login quebra com acento"); got != "fix/12-login-quebra-com-acento" {
		t.Fatalf("bug branch: %q", got)
	}
	if got := BranchName("feature", "task:add-x", "Add export"); got != "feat/task-add-x-add-export" {
		t.Fatalf("feature branch: %q", got)
	}
}

func TestBranchNameShouldKeepDistinctTaskRefsDistinct(t *testing.T) {
	a := BranchName("bug", "task:fix-add", "Corrigir soma")
	b := BranchName("bug", "task:fix-add-v2", "Corrigir soma")
	if a == b {
		t.Fatalf("different tasks must not share a branch: %q", a)
	}
}

func TestGuardWorkBranchShouldRefuseProtectedAndBaseBranches(t *testing.T) {
	for _, b := range []string{"main", "master", "develop", "", "HEAD"} {
		if err := GuardWorkBranch(b, "origin/develop"); err == nil {
			t.Errorf("%q must be refused as a work branch", b)
		}
	}
	if err := GuardWorkBranch("feat/12-x", "origin/main"); err != nil {
		t.Fatalf("a feature branch is allowed: %v", err)
	}
}

// repo creates a real git repository with one commit on main.
func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestResolveBaseShouldUseLocalBranchWhenThereIsNoRemote(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	base, err := g.ResolveBase(ctx, r)
	if err != nil || base != "main" {
		t.Fatalf("got %q %v", base, err)
	}
	if g.HasRemote(ctx, r, "origin") {
		t.Fatal("fresh repo has no origin")
	}
}

func TestCommitsAndChangedFilesShouldReflectWorkSinceBase(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	if cs, err := g.CommitsSince(ctx, r, "main"); err != nil || len(cs) != 0 {
		t.Fatalf("no work yet: %v %v", cs, err)
	}
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", r}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("switch", "-q", "-c", "feat/x")
	if err := os.WriteFile(filepath.Join(r, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Status(ctx, r); st == "" {
		t.Fatal("untracked file must make status dirty")
	}
	run("add", "a.go")
	run("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "feat: add a")
	cs, _ := g.CommitsSince(ctx, r, "main")
	files, _ := g.ChangedFiles(ctx, r, "main")
	if len(cs) != 1 || len(files) != 1 || files[0] != "a.go" {
		t.Fatalf("commits=%v files=%v", cs, files)
	}
	if b, _ := g.CurrentBranch(ctx, r); b != "feat/x" {
		t.Fatalf("branch %q", b)
	}
	if !g.BranchExists(ctx, r, "feat/x") || g.BranchExists(ctx, r, "nope") {
		t.Fatal("BranchExists wrong")
	}
}

// withOrigin gives repo r a bare origin and a pushed-nothing state.
func withOrigin(t *testing.T, r string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "origin.git")
	for _, args := range [][]string{{"init", "-q", "--bare", bare}, {"-C", r, "remote", "add", "origin", bare}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return bare
}

func TestPushShouldPublishWorkBranchWithoutForce(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	if out, err := exec.Command("git", "-C", r, "switch", "-q", "-c", "feat/x").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if ok, err := g.RemoteBranchExists(ctx, r, "feat/x"); err != nil || ok {
		t.Fatalf("not pushed yet: %v %v", ok, err)
	}
	if err := g.Push(ctx, r, "feat/x", "origin/main"); err != nil {
		t.Fatal(err)
	}
	if ok, err := g.RemoteBranchExists(ctx, r, "feat/x"); err != nil || !ok {
		t.Fatalf("branch must be on origin: %v %v", ok, err)
	}
}

func TestPushShouldRefuseProtectedBranchesBeforeTouchingTheRemote(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	for _, b := range []string{"main", "master", "develop"} {
		if err := g.Push(ctx, r, b, "origin/develop"); err == nil {
			t.Errorf("push of %q must be refused", b)
		}
	}
	if ok, _ := g.RemoteBranchExists(ctx, r, "main"); ok {
		t.Fatal("main must not reach origin")
	}
}

func TestAgentEnvShouldBlockEveryPushButKeepFetch(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	bare := withOrigin(t, r)
	for _, remote := range []string{"origin", "https://github.com/acme/none.git", "git@github.com:acme/none.git", "ssh://git@gitlab.com/acme/none.git"} {
		cmd := exec.CommandContext(ctx, "git", "-C", r, "push", remote, "main")
		cmd.Env = append(os.Environ(), append(AgentEnv(), "GIT_TERMINAL_PROMPT=0")...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("agent push to %s must fail: %s", remote, out)
		}
	}
	if out, err := exec.Command("git", "--git-dir", bare, "for-each-ref").Output(); err != nil || len(out) != 0 {
		t.Fatalf("origin must stay empty, refs: %s %v", out, err)
	}
	fetch := exec.CommandContext(ctx, "git", "-C", r, "fetch", "-q", "origin")
	fetch.Env = append(os.Environ(), AgentEnv()...)
	if out, err := fetch.CombinedOutput(); err != nil {
		t.Fatalf("agents still need fetch: %v %s", err, out)
	}
}

func TestRedProofWorktreeShouldCarryOnlyTheBranchTestsOntoBase(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", r, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("switch", "-q", "-c", "fix/x")
	for name, body := range map[string]string{"calc.go": "fixed", "calc_test.go": "test"} {
		if err := os.WriteFile(filepath.Join(r, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "calc.go", "calc_test.go")
	run("commit", "-q", "-m", "fix: x")

	dir := filepath.Join(t.TempDir(), "redproof")
	if err := g.AddDetachedWorktree(ctx, r, dir, "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.CheckoutFiles(ctx, dir, "fix/x", []string{"calc_test.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "calc.go")); !os.IsNotExist(err) {
		t.Fatal("the fix must not reach the base worktree")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "calc_test.go")); string(b) != "test" {
		t.Fatalf("the branch test must be applied, got %q", b)
	}
	if err := g.RemoveWorktree(ctx, r, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("temporary worktree must be gone")
	}
	if b, _ := g.CurrentBranch(ctx, r); b != "fix/x" {
		t.Fatalf("the work branch is untouched, got %q", b)
	}
}

func TestDiscardEmptyWorkShouldRemoveWorktreeAndBranchWithoutCommits(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", r, "worktree", "add", "-q", "-b", "fix/empty", wt, "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := g.DiscardEmptyWork(ctx, r, wt, "fix/empty", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) || g.BranchExists(ctx, r, "fix/empty") {
		t.Fatal("an empty halted run must leave nothing behind")
	}
}

func TestDiscardEmptyWorkShouldRefuseBranchesWithCommits(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	for _, args := range [][]string{
		{"-C", r, "worktree", "add", "-q", "-b", "fix/work", wt, "main"},
		{"-C", wt, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "fix: work"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	if err := g.DiscardEmptyWork(ctx, r, wt, "fix/work", "main"); err == nil {
		t.Fatal("committed work must never be discarded")
	}
	if _, err := os.Stat(wt); err != nil || !g.BranchExists(ctx, r, "fix/work") {
		t.Fatal("worktree and branch must survive")
	}
}

func TestDiscardEmptyWorkShouldKeepUncommittedChanges(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", r, "worktree", "add", "-q", "-b", "fix/dirty", wt, "main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_ = os.WriteFile(filepath.Join(wt, "wip.go"), []byte("package x"), 0o600)
	if err := g.DiscardEmptyWork(ctx, r, wt, "fix/dirty", "main"); err == nil {
		t.Fatal("uncommitted agent work must not be thrown away")
	}
	if _, err := os.Stat(filepath.Join(wt, "wip.go")); err != nil || !g.BranchExists(ctx, r, "fix/dirty") {
		t.Fatal("dirty worktree and its branch must survive")
	}
}
