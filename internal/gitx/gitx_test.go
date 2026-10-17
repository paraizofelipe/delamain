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
