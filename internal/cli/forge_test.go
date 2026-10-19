package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync"
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

// fakeTokens is a hermetic forge.TokenLookup: env is the environment and no forge CLI exists.
type fakeTokens struct {
	env   map[string]string
	mu    sync.Mutex
	execs []string
}

func (f *fakeTokens) Getenv(k string) string { return f.env[k] }

func (f *fakeTokens) Exec(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, strings.Join(append([]string{name}, args...), " "))
	return "", exec.ErrNotFound
}

// useTokens makes forgeFor resolve tokens from env only, for the duration of the test.
func useTokens(t *testing.T, env map[string]string) *fakeTokens {
	t.Helper()
	f := &fakeTokens{env: env}
	prev := tokenLookup
	tokenLookup = f
	t.Cleanup(func() { tokenLookup = prev })
	return f
}

func TestForgeForShouldDetectForgeFromOrigin(t *testing.T) {
	useTokens(t, map[string]string{"GITLAB_TOKEN": "glpat-test"})
	cfg := config.Default()
	f, err := forgeFor(context.Background(), cfg, gitRepo(t, "git@gitlab.com:grp/api.git"))
	if err != nil || f == nil {
		t.Fatalf("forge = %v, err = %v", f, err)
	}
	if got := f.Repo(); got != (forge.Repo{Kind: forge.GitLab, Host: "gitlab.com", Path: "grp/api"}) {
		t.Fatalf("repo = %+v", got)
	}
}

func TestForgeForShouldNotRunTheForgeCLIWhenTheTokenIsInTheEnv(t *testing.T) {
	tokens := useTokens(t, map[string]string{"GH_TOKEN": "ghp-test"})
	f, err := forgeFor(context.Background(), config.Default(), gitRepo(t, "https://github.com/acme/api.git"))
	if err != nil || f == nil {
		t.Fatalf("forge = %v, err = %v", f, err)
	}
	if got := f.Repo(); got != (forge.Repo{Kind: forge.GitHub, Host: "github.com", Path: "acme/api"}) {
		t.Fatalf("repo = %+v", got)
	}
	if len(tokens.execs) != 0 {
		t.Fatalf("a token in the env must not run the forge CLI: %v", tokens.execs)
	}
}

func TestForgeForShouldExplainTheTokenVariablesWhenGitHubHasNoToken(t *testing.T) {
	useTokens(t, nil)
	_, err := forgeFor(context.Background(), config.Default(), gitRepo(t, "git@github.com:acme/api.git"))
	if !errors.Is(err, forge.ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
	for _, want := range []string{"github.com", "GH_TOKEN", "GITHUB_TOKEN", "gh auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}

	cfg := config.Default()
	cfg.Forge.Kind = config.ForgeGitHub
	_, err = forgeFor(context.Background(), cfg, gitRepo(t, "https://ghe.corp.example/team/api.git"))
	for _, want := range []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "gh auth login --hostname ghe.corp.example"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("enterprise error %v must mention %q", err, want)
		}
	}
}

func TestForgeForShouldExplainTheTokenVariablesWhenGitLabHasNoToken(t *testing.T) {
	useTokens(t, map[string]string{"GH_TOKEN": "not-for-gitlab"})
	_, err := forgeFor(context.Background(), config.Default(), gitRepo(t, "git@gitlab.com:grp/api.git"))
	if !errors.Is(err, forge.ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
	for _, want := range []string{"gitlab.com", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "glab auth login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestForgeForShouldKeepWorkLocalWithoutOrigin(t *testing.T) {
	useTokens(t, nil)
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
	useTokens(t, nil)
	cfg := config.Default()
	cfg.Forge.Kind = config.ForgeNone
	if f, err := forgeFor(context.Background(), cfg, gitRepo(t, "https://github.com/acme/api.git")); err != nil || f != nil {
		t.Fatalf("forge.kind=none must opt out: %v %v", f, err)
	}
}

func TestForgeUserShouldReturnLoginAndSourceWithoutTheToken(t *testing.T) {
	const secret = "ghe_S3CR3T_FOR_DOCTOR"
	useTokens(t, map[string]string{"GH_ENTERPRISE_TOKEN": secret})
	var path, auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	t.Cleanup(srv.Close)
	prev := forgeOptions
	forgeOptions = []forge.Option{forge.WithHTTPClient(srv.Client())}
	t.Cleanup(func() { forgeOptions = prev })

	login, source, err := forgeUser(context.Background(), forge.Repo{Kind: forge.GitHub, Host: srv.Listener.Addr().String(), Path: "team/api"})
	if err != nil || login != "octocat" || source != "GH_ENTERPRISE_TOKEN" {
		t.Fatalf("login=%q source=%q err=%v", login, source, err)
	}
	if strings.Contains(login+source, secret) {
		t.Fatal("the token value must never be reported")
	}
	if path != "/api/v3/user" || auth != "Bearer "+secret {
		t.Fatalf("GET %s with %q", path, auth)
	}
}

func TestForgeReposShouldListDaemonReposWithAForgeRemote(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.Repos = []string{gitRepo(t, "git@github.com:acme/api.git"), gitRepo(t, ""), gitRepo(t, "https://gitlab.com/grp/sub/web.git")}
	repos, err := forgeRepos(context.Background(), cfg)
	want := []forge.Repo{{Kind: forge.GitHub, Host: "github.com", Path: "acme/api"}, {Kind: forge.GitLab, Host: "gitlab.com", Path: "grp/sub/web"}}
	if err != nil || !slices.Equal(repos, want) {
		t.Fatalf("repos = %+v, err = %v; repos without origin are skipped", repos, err)
	}
	cfg.Forge.Kind = config.ForgeNone
	if repos, err := forgeRepos(context.Background(), cfg); err != nil || len(repos) != 0 {
		t.Fatalf("forge.kind=none checks nothing: %+v %v", repos, err)
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
	if err != nil || !strings.Contains(out, "no orphaned run") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
