package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/prompts"
)

type fakeHerdr struct {
	sessions    []herdr.Session
	sessionsErr error
}

func (f fakeHerdr) Sessions(context.Context) ([]herdr.Session, error) {
	return f.sessions, f.sessionsErr
}

func healthy(t *testing.T) (config.Config, Deps) {
	cfg := config.Default()
	cfg.Paths.StateDir = t.TempDir()
	cfg.Herdr.Enabled = true
	return cfg, Deps{
		LookPath: func(b string) (string, error) { return "/usr/bin/" + b, nil },
		Version:  func(_ context.Context, b string) (string, error) { return filepath.Base(b) + " 1.0\nextra", nil },
		OmpRPC:   func(context.Context, string) error { return nil },
		Herdr:    fakeHerdr{sessions: []herdr.Session{{Name: "default", Default: true, Running: true}}},
	}
}

func byName(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

func TestRunShouldPassWhenToolchainIsComplete(t *testing.T) {
	cfg, d := healthy(t)
	checks := Run(context.Background(), cfg, d)
	if Failed(checks) {
		t.Fatalf("healthy environment must pass: %+v", checks)
	}
	if got := byName(checks)["omp"].Detail; got != "omp 1.0 (/usr/bin/omp)" {
		t.Fatalf("version detail should be the first line only: %q", got)
	}
}

func TestRunShouldFailWhenOmpDoesNotBecomeReadyInRPCMode(t *testing.T) {
	cfg, d := healthy(t)
	var got string
	d.OmpRPC = func(_ context.Context, bin string) error {
		got = bin
		return errors.New("omp não ficou pronto em 15s")
	}
	c := byName(Run(context.Background(), cfg, d))["omp rpc"]
	// Every agent runs over RPC: without it no run can start.
	if c.Status != Fail || c.Detail != "omp não ficou pronto em 15s" {
		t.Fatalf("rpc failure must fail doctor with its reason: %+v", c)
	}
	if got != "/usr/bin/omp" {
		t.Fatalf("the rpc check must use the resolved omp binary, got %q", got)
	}
}

func TestRunShouldNotStartOmpRPCWhenOmpIsMissing(t *testing.T) {
	cfg, d := healthy(t)
	d.LookPath = func(b string) (string, error) {
		if b == cfg.Omp.Binary {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	d.OmpRPC = func(context.Context, string) error {
		t.Fatal("omp rpc must not run without an omp binary")
		return nil
	}
	if c := byName(Run(context.Background(), cfg, d))["omp rpc"]; c.Status != Fail {
		t.Fatalf("a missing omp cannot be reported as rpc-ready: %+v", c)
	}
}

func claudeHarness(t *testing.T) (config.Config, Deps) {
	cfg, d := healthy(t)
	cfg.Agent.Harness = config.HarnessClaude
	d.Version = func(_ context.Context, b string) (string, error) {
		if filepath.Base(b) == "claude" {
			return "2.1.303 (Claude Code)", nil
		}
		return filepath.Base(b) + " 1.0", nil
	}
	d.ClaudeAuth = func(context.Context, string) error { return nil }
	d.OmpRPC = func(context.Context, string) error {
		t.Fatal("the claude harness never needs omp")
		return nil
	}
	return cfg, d
}

func TestRunShouldCheckClaudeInsteadOfOmpWhenItIsTheHarness(t *testing.T) {
	cfg, d := claudeHarness(t)
	d.LookPath = func(b string) (string, error) {
		if b == cfg.Omp.Binary {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	results := Run(context.Background(), cfg, d)
	checks := byName(results)
	if Failed(results) {
		t.Fatalf("omp is not needed with harness=claude: %+v", checks)
	}
	if _, ok := checks["omp"]; ok {
		t.Fatalf("omp must not be checked: %+v", checks)
	}
	if c := checks["claude"]; c.Status != OK || c.Detail != "2.1.303 (Claude Code) (/usr/bin/claude)" {
		t.Fatalf("claude: %+v", c)
	}
	if c := checks["claude auth"]; c.Status != OK {
		t.Fatalf("claude auth: %+v", c)
	}
}

func TestRunShouldFailWhenClaudeIsNotLoggedInAndWarnOnAnOldClaude(t *testing.T) {
	cfg, d := claudeHarness(t)
	d.ClaudeAuth = func(context.Context, string) error { return errors.New("not logged in") }
	if c := byName(Run(context.Background(), cfg, d))["claude auth"]; c.Status != Fail || c.Detail != "not logged in (run: claude auth login)" {
		t.Fatalf("an agent without credentials fails its first turn: %+v", c)
	}
	cfg, d = claudeHarness(t)
	d.Version = func(_ context.Context, b string) (string, error) { return "2.1.100 (Claude Code)", nil }
	if c := byName(Run(context.Background(), cfg, d))["claude"]; c.Status != Warn {
		t.Fatalf("a claude older than %s must be flagged: %+v", MinClaude, c)
	}
}

func TestOlderThanShouldCompareXYZAndIgnoreAPreReleaseSuffix(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"2.1.100 (Claude Code)", true},
		{"2.1.290 (Claude Code)", false},
		{"2.1.303 (Claude Code)", false},
		{"2.1.100-beta (Claude Code)", true},
		{"2.1.303-rc.1 (Claude Code)", false},
		{"", false},
		{"not a version", false},
	} {
		if got := olderThan(tc.version, MinClaude); got != tc.want {
			t.Errorf("olderThan(%q, %s) = %v, want %v", tc.version, MinClaude, got, tc.want)
		}
	}
}

func TestRunShouldFailWhenConfiguredSessionIsStoppedOrAbsent(t *testing.T) {
	cfg, d := healthy(t)
	cfg.Herdr.Session = "delamain-test"
	d.Herdr = fakeHerdr{sessions: []herdr.Session{{Name: "delamain-test", Running: false}}}
	if c := byName(Run(context.Background(), cfg, d))["herdr server"]; c.Status != Fail {
		t.Fatalf("stopped session: %+v", c)
	}
	cfg.Herdr.Session = "ghost"
	if c := byName(Run(context.Background(), cfg, d))["herdr server"]; c.Status != Fail {
		t.Fatalf("absent session: %+v", c)
	}
}

func TestRunShouldNotRequireHerdrWhenItIsOff(t *testing.T) {
	cfg, d := healthy(t)
	cfg.Herdr.Enabled = false
	d.LookPath = func(b string) (string, error) {
		if b == "herdr" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	d.Herdr = fakeHerdr{sessionsErr: errors.New("socket down")}
	m := byName(Run(context.Background(), cfg, d))
	if m["herdr"].Status != OK || m["herdr server"] != (Check{}) {
		t.Fatalf("herdr is optional and off: %+v %+v", m["herdr"], m["herdr server"])
	}
	if m["omp rpc"].Status != OK || m["git"].Status != OK {
		t.Fatalf("the headless flow still needs omp and git: %+v", m)
	}
}

func TestRunShouldReportEveryProblemInsteadOfStoppingAtFirst(t *testing.T) {
	cfg, d := healthy(t)
	d.LookPath = func(b string) (string, error) { return "", errors.New("not found") }
	d.Herdr = fakeHerdr{sessionsErr: errors.New("socket down")}
	m := byName(Run(context.Background(), cfg, d))
	for _, name := range []string{"herdr", "omp", "omp rpc", "git", "herdr server"} {
		if m[name].Status != Fail {
			t.Errorf("%s should fail: %+v", name, m[name])
		}
	}
	if m["gitleaks"].Status != Warn {
		t.Errorf("optional tools are warnings, not failures: %+v", m["gitleaks"])
	}
}

func TestRunShouldFailWhenStateDirIsNotWritable(t *testing.T) {
	cfg, d := healthy(t)
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	cfg.Paths.StateDir = filepath.Join(ro, "state")
	if c := byName(Run(context.Background(), cfg, d))["state dir"]; c.Status != Fail {
		t.Fatalf("unwritable state dir must fail: %+v", c)
	}
}

func TestFailedShouldIgnoreWarnings(t *testing.T) {
	if Failed([]Check{{Status: OK}, {Status: Warn}}) {
		t.Fatal("warnings must not fail doctor")
	}
	if !Failed([]Check{{Status: OK}, {Status: Fail}}) {
		t.Fatal("any failure fails doctor")
	}
}

func validPrompts(t *testing.T) *prompts.Set {
	t.Helper()
	s, err := prompts.Load(fstest.MapFS{
		"manifest.yaml":           {Data: []byte("roles: [planner]\nagents: [analyzer]\nskills: [tdd]\nstages: [plan]\n")},
		"roles/planner/body.md":   {Data: []byte("planner\n")},
		"agents/analyzer/omp.yml": {Data: []byte("name: delamain-analyzer\ndescription: d\n")},
		"agents/analyzer/body.md": {Data: []byte("analyzer\n")},
		"skills/tdd/SKILL.md":     {Data: []byte("---\nname: delamain-tdd\ndescription: d\n---\nbody\n")},
		"stages/plan.md":          {Data: []byte("{{.ArtifactPath}}")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunShouldFailUntilPromptsAreInstalledAndPassAfter(t *testing.T) {
	cfg, d := healthy(t)
	d.Prompts = validPrompts(t)
	d.PromptsTarget = prompts.Target{OmpAgentDir: t.TempDir(), RolesDir: t.TempDir()}
	if c := byName(Run(context.Background(), cfg, d))["prompts"]; c.Status != Fail {
		t.Fatalf("uninstalled prompts would leave omp without delamain agents: %+v", c)
	}
	if _, err := d.Prompts.Install(d.PromptsTarget); err != nil {
		t.Fatal(err)
	}
	if c := byName(Run(context.Background(), cfg, d))["prompts"]; c.Status != OK {
		t.Fatalf("installed prompts must pass: %+v", c)
	}
}

func TestRunShouldWarnWhenInstalledPromptsAreOutdated(t *testing.T) {
	cfg, d := healthy(t)
	d.Prompts = validPrompts(t)
	d.PromptsTarget = prompts.Target{OmpAgentDir: t.TempDir(), RolesDir: t.TempDir()}
	if _, err := d.Prompts.Install(d.PromptsTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.PromptsTarget.RolePath("planner"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := byName(Run(context.Background(), cfg, d))["prompts"]; c.Status != Warn {
		t.Fatalf("outdated install is a warning: %+v", c)
	}
}

var (
	ghRepo = forge.Repo{Kind: forge.GitHub, Host: "github.com", Path: "acme/api"}
	glRepo = forge.Repo{Kind: forge.GitLab, Host: "gitlab.corp.example", Path: "grp/sub/api"}
)

// withForge makes doctor check repos, answering each with user.
func withForge(d Deps, repos []forge.Repo, user func(forge.Repo) (string, string, error)) Deps {
	d.ForgeRepos = func(context.Context) ([]forge.Repo, error) { return repos, nil }
	d.ForgeUser = func(_ context.Context, r forge.Repo) (string, string, error) { return user(r) }
	return d
}

// authChecks returns the "forge auth" checks, one per repository.
func authChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Name == "forge auth" {
			out = append(out, c)
		}
	}
	return out
}

func TestRunShouldShowForgeLoginHostAndTokenSourceWhenAuthenticated(t *testing.T) {
	cfg, d := healthy(t)
	d = withForge(d, []forge.Repo{ghRepo}, func(forge.Repo) (string, string, error) { return "octocat", "GH_TOKEN", nil })
	checks := Run(context.Background(), cfg, d)
	got := authChecks(checks)
	if len(got) != 1 || got[0].Status != OK || !strings.Contains(got[0].Detail, "octocat on github.com (token from GH_TOKEN)") {
		t.Fatalf("forge auth = %+v", got)
	}
	if Failed(checks) {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestRunShouldWarnWithLoginHintWhenForgeHasNoToken(t *testing.T) {
	cfg, d := healthy(t)
	d = withForge(d, []forge.Repo{ghRepo, glRepo}, func(r forge.Repo) (string, string, error) {
		return "", "", fmt.Errorf("%w for %s", forge.ErrTokenNotFound, r.Host)
	})
	checks := Run(context.Background(), cfg, d)
	got := authChecks(checks)
	if len(got) != 2 || Failed(checks) {
		t.Fatalf("no token is a warning: %+v", checks)
	}
	for i, wants := range [][]string{
		{"acme/api", "GH_TOKEN", "GITHUB_TOKEN", "gh auth login", "Issues", "Pull requests"},
		{"grp/sub/api", "GITLAB_TOKEN", "glab auth login --hostname gitlab.corp.example", "api scope"},
	} {
		if got[i].Status != Warn {
			t.Errorf("status = %+v", got[i])
		}
		for _, w := range wants {
			if !strings.Contains(got[i].Detail, w) {
				t.Errorf("detail %q must mention %q", got[i].Detail, w)
			}
		}
	}
}

func TestRunShouldFailWhenForgeRejectsTheToken(t *testing.T) {
	for _, status := range []int{401, 403} {
		cfg, d := healthy(t)
		d = withForge(d, []forge.Repo{ghRepo}, func(forge.Repo) (string, string, error) {
			return "", "GH_TOKEN", &forge.APIError{Forge: forge.GitHub, Op: "user", Status: status, Message: "Bad credentials"}
		})
		checks := Run(context.Background(), cfg, d)
		got := authChecks(checks)
		if len(got) != 1 || got[0].Status != Fail || !Failed(checks) || !strings.Contains(got[0].Detail, "GH_TOKEN") || !strings.Contains(got[0].Detail, "Bad credentials") {
			t.Fatalf("%d: a rejected token fails doctor: %+v", status, got)
		}
	}
}

func TestRunShouldWarnWhenForgeIsUnreachable(t *testing.T) {
	cfg, d := healthy(t)
	d = withForge(d, []forge.Repo{ghRepo}, func(forge.Repo) (string, string, error) {
		return "", "GH_TOKEN", &forge.APIError{Forge: forge.GitHub, Op: "user", Message: "dial tcp: no such host"}
	})
	checks := Run(context.Background(), cfg, d)
	got := authChecks(checks)
	if len(got) != 1 || got[0].Status != Warn || Failed(checks) || !strings.Contains(got[0].Detail, "no such host") {
		t.Fatalf("an offline machine is a warning: %+v", got)
	}
}

func TestRunShouldCheckForgeAuthForEveryDaemonRepo(t *testing.T) {
	cfg, d := healthy(t)
	var asked []string
	d = withForge(d, []forge.Repo{ghRepo, glRepo}, func(r forge.Repo) (string, string, error) {
		asked = append(asked, r.String())
		return "bot", "GITLAB_TOKEN", nil
	})
	got := authChecks(Run(context.Background(), cfg, d))
	if len(got) != 2 || len(asked) != 2 || !strings.Contains(got[0].Detail, "acme/api") || !strings.Contains(got[1].Detail, "grp/sub/api") {
		t.Fatalf("one check per repository: %+v asked=%v", got, asked)
	}

	d = withForge(d, nil, nil)
	got = authChecks(Run(context.Background(), cfg, d))
	if len(got) != 1 || got[0].Status != Warn || !strings.Contains(got[0].Detail, "no repository with a forge remote") {
		t.Fatalf("nothing to check is said, not hidden: %+v", got)
	}
}

func TestRunShouldNotFailWithoutGhOrGlabInstalled(t *testing.T) {
	cfg, d := healthy(t)
	d.LookPath = func(b string) (string, error) {
		if b == "gh" || b == "glab" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	d = withForge(d, []forge.Repo{ghRepo}, func(forge.Repo) (string, string, error) { return "octocat", "GH_TOKEN", nil })
	checks := Run(context.Background(), cfg, d)
	if Failed(checks) {
		t.Fatalf("the forge CLIs are optional: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != OK && (strings.Contains(c.Detail, "gh ") || strings.Contains(c.Detail, "glab")) {
			t.Errorf("gh/glab are not requirements: %+v", c)
		}
	}
}
