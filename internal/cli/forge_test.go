package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
)

func gitRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	cmds := [][]string{{"init", "-q", "-b", "main"}}
	if origin != "" {
		cmds = append(cmds, []string{"remote", "add", "origin", origin})
	}
	for _, args := range cmds {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestForgeForShouldDetectForgeFromOrigin(t *testing.T) {
	cfg := config.Default()
	f, err := forgeFor(context.Background(), cfg, gitRepo(t, "git@gitlab.com:grp/api.git"))
	if err != nil || f == nil {
		t.Fatalf("forge = %v, err = %v", f, err)
	}
	if got := f.Repo(); got != (forge.Repo{Kind: forge.GitLab, Host: "gitlab.com", Path: "grp/api"}) {
		t.Fatalf("repo = %+v", got)
	}
}

func TestForgeForShouldKeepWorkLocalWithoutOrigin(t *testing.T) {
	f, err := forgeFor(context.Background(), config.Default(), gitRepo(t, ""))
	if err != nil || f != nil {
		t.Fatalf("no origin means local delivery, got %v %v", f, err)
	}
}

func TestForgeForShouldFailWhenForgeIsForcedButOriginIsMissing(t *testing.T) {
	cfg := config.Default()
	cfg.Forge.Kind = config.ForgeGitHub
	if _, err := forgeFor(context.Background(), cfg, gitRepo(t, "")); err == nil {
		t.Fatal("an explicit forge without origin is a misconfiguration")
	}
}

func TestForgeForShouldFailOnUnknownForgeHost(t *testing.T) {
	if _, err := forgeFor(context.Background(), config.Default(), gitRepo(t, "https://bitbucket.org/acme/api.git")); err == nil {
		t.Fatal("unknown hosts must not silently skip the PR")
	}
}

func TestForgeForShouldHonorForgeNone(t *testing.T) {
	cfg := config.Default()
	cfg.Forge.Kind = config.ForgeNone
	if f, err := forgeFor(context.Background(), cfg, gitRepo(t, "https://github.com/acme/api.git")); err != nil || f != nil {
		t.Fatalf("forge.kind=none must opt out: %v %v", f, err)
	}
}

func TestRunShouldRequireExactlyOneOfTaskOrIssue(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "--task", "x.md", "--issue", "3"}} {
		if _, err := execute(t, args...); err == nil {
			t.Errorf("%v must be rejected", args)
		}
	}
}

func TestLockDaemonShouldAllowOnlyOneInstancePerStateDir(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDaemon(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDaemon(dir); err == nil {
		t.Fatal("a second daemon must be refused")
	}
	unlock()
	again, err := lockDaemon(dir)
	if err != nil {
		t.Fatalf("lock must be released on exit: %v", err)
	}
	again()
}

func TestDaemonShouldRequireRepositories(t *testing.T) {
	t.Setenv("DELAMAIN_STATE_DIR", t.TempDir())
	if _, err := execute(t, "daemon", "--config", "/nonexistent.yaml"); err == nil {
		t.Fatal("a daemon without repositories must refuse to start")
	}
}

func TestDaemonShouldRefuseRepositoriesWithoutForge(t *testing.T) {
	t.Setenv("DELAMAIN_STATE_DIR", t.TempDir())
	if _, err := execute(t, "daemon", "--config", "/nonexistent.yaml", "--repo", gitRepo(t, "")); err == nil {
		t.Fatal("polling needs a forge")
	}
}

func TestRecoverShouldReportWhenNothingIsOrphaned(t *testing.T) {
	t.Setenv("DELAMAIN_STATE_DIR", t.TempDir())
	out, err := execute(t, "recover", "--config", "/nonexistent.yaml")
	if err != nil || !strings.Contains(out, "nenhuma run órfã") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
