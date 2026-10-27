package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/notify"
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
		Version: func(_ context.Context, b string, _ []string, _ ...string) (string, error) {
			return filepath.Base(b) + " 1.0\nextra", nil
		},
		OmpRPC: func(context.Context, string) error { return nil },
		Herdr:  fakeHerdr{sessions: []herdr.Session{{Name: "default", Default: true, Running: true}}},
		ForgeAuth: func(context.Context, string) (ForgeIdentity, error) {
			return ForgeIdentity{Kind: forge.GitHub, Host: "github.com", Login: "delamain-bot", Source: "env GH_TOKEN"}, nil
		},
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
	d.Version = func(_ context.Context, b string, _ []string, _ ...string) (string, error) {
		if filepath.Base(b) == "claude" {
			return "2.1.303 (Claude Code)", nil
		}
		return filepath.Base(b) + " 1.0", nil
	}
	d.ClaudeAuth = func(context.Context, string, []string, ...string) error { return nil }
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
	d.ClaudeAuth = func(context.Context, string, []string, ...string) error { return errors.New("not logged in") }
	if c := byName(Run(context.Background(), cfg, d))["claude auth"]; c.Status != Fail || c.Detail != "not logged in (run: claude auth login)" {
		t.Fatalf("an agent without credentials fails its first turn: %+v", c)
	}
	cfg, d = claudeHarness(t)
	d.Version = func(_ context.Context, b string, _ []string, _ ...string) (string, error) {
		return "2.1.100 (Claude Code)", nil
	}
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
	d.ForgeAuth = func(context.Context, string) (ForgeIdentity, error) {
		return ForgeIdentity{Kind: forge.GitHub, Host: "github.com"}, forge.ErrTokenNotFound
	}
	d.LookPath = func(b string) (string, error) { return "", errors.New("not found") }
	d.Herdr = fakeHerdr{sessionsErr: errors.New("socket down")}
	m := byName(Run(context.Background(), cfg, d))
	for _, name := range []string{"herdr", "omp", "omp rpc", "git", "herdr server", "forge auth"} {
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

func forgeAuthChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Name == "forge auth" {
			out = append(out, c)
		}
	}
	return out
}

func TestRunShouldReportForgeAuthLoginHostAndSourceWithoutTheToken(t *testing.T) {
	cfg, d := healthy(t)
	got := forgeAuthChecks(Run(context.Background(), cfg, d))
	if len(got) != 1 || got[0].Status != OK {
		t.Fatalf("forge auth = %+v", got)
	}
	for _, want := range []string{"delamain-bot", "github.com", "env GH_TOKEN"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail %q must mention %q", got[0].Detail, want)
		}
	}
}

func TestRunShouldFailWhenNoForgeTokenIsFound(t *testing.T) {
	cfg, d := healthy(t)
	d.ForgeAuth = func(context.Context, string) (ForgeIdentity, error) {
		return ForgeIdentity{Kind: forge.GitHub, Host: "github.com"}, fmt.Errorf("no github token for github.com: %w; set GH_TOKEN or GITHUB_TOKEN", forge.ErrTokenNotFound)
	}
	got := forgeAuthChecks(Run(context.Background(), cfg, d))
	if len(got) != 1 || got[0].Status != Fail {
		t.Fatalf("a missing token fails: run and daemon stop on that repository without one: %+v", got)
	}
	for _, want := range []string{"GH_TOKEN", "Pull requests", "runs on this repository fail"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail %q must explain how to provide a token (%q)", got[0].Detail, want)
		}
	}
}

func TestRunShouldFailWhenTheForgeRejectsTheToken(t *testing.T) {
	cases := map[error]Status{
		&forge.APIError{Forge: forge.GitLab, Op: "whoami", Status: 401, Message: "401 Unauthorized"}:   Fail,
		&forge.APIError{Forge: forge.GitLab, Op: "whoami", Status: 403, Message: "insufficient_scope"}: Fail,
		&forge.APIError{Forge: forge.GitLab, Op: "whoami", Status: 503, Message: "unavailable"}:        Warn,
		errors.New("gitlab whoami: dial tcp: no such host"):                                            Warn,
	}
	for err, want := range cases {
		cfg, d := healthy(t)
		d.ForgeAuth = func(context.Context, string) (ForgeIdentity, error) {
			return ForgeIdentity{Kind: forge.GitLab, Host: "gitlab.com", Source: "env GITLAB_TOKEN"}, err
		}
		got := forgeAuthChecks(Run(context.Background(), cfg, d))
		if len(got) != 1 || got[0].Status != want {
			t.Errorf("%v: forge auth = %+v, want %s", err, got, want)
			continue
		}
		if want == Fail && !strings.Contains(got[0].Detail, "api scope") {
			t.Errorf("a rejected token must say which permissions are needed: %q", got[0].Detail)
		}
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
	checks := Run(context.Background(), cfg, d)
	if Failed(checks) {
		t.Fatalf("gh/glab are optional: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != OK {
			t.Errorf("nothing should even warn about gh/glab: %+v", c)
		}
	}
}

func TestRunShouldCheckForgeAuthForEveryDaemonRepo(t *testing.T) {
	cfg, d := healthy(t)
	var asked []string
	d.ForgeAuth = func(_ context.Context, dir string) (ForgeIdentity, error) {
		asked = append(asked, dir)
		if dir == "/srv/local" {
			return ForgeIdentity{}, nil // no origin: work stays local
		}
		return ForgeIdentity{Kind: forge.GitHub, Host: "github.com", Login: "bot", Source: "gh auth token"}, nil
	}
	cfg.Daemon.Repos = []string{"/srv/api", "/srv/local"}
	got := forgeAuthChecks(Run(context.Background(), cfg, d))
	if strings.Join(asked, ",") != "/srv/api,/srv/local" || len(got) != 2 {
		t.Fatalf("asked %v, checks %+v", asked, got)
	}
	if got[0].Status != OK || !strings.Contains(got[0].Detail, "/srv/api") || got[1].Status != OK || !strings.Contains(got[1].Detail, "local") {
		t.Fatalf("checks = %+v", got)
	}

	asked = nil
	cfg.Daemon.Repos = nil
	Run(context.Background(), cfg, d)
	if strings.Join(asked, ",") != "." {
		t.Fatalf("without daemon repos the current directory is checked, asked %v", asked)
	}
}

func TestRunShouldReportTheNotificationServerOrWarnWithoutOne(t *testing.T) {
	cfg, d := healthy(t)
	d.Notify = func(context.Context) notify.Probe {
		return notify.Probe{Backend: "dbus", Server: "mako", Version: "1.9.0", Actions: true}
	}
	c := byName(Run(context.Background(), cfg, d))["notify"]
	if c.Status != OK || c.Detail != "dbus: mako 1.9.0, actions supported" {
		t.Fatalf("notify check = %+v", c)
	}

	d.Notify = func(context.Context) notify.Probe {
		return notify.Probe{Backend: "dbus", Server: "dunst", Version: "1.11", Actions: false}
	}
	if c := byName(Run(context.Background(), cfg, d))["notify"]; c.Status != OK || c.Detail != "dbus: dunst 1.11, no actions (clicks disabled)" {
		t.Fatalf("notify check = %+v", c)
	}

	d.Notify = func(context.Context) notify.Probe {
		return notify.Probe{Backend: "dbus", Err: errors.New("org.freedesktop.Notifications: timeout")}
	}
	if c := byName(Run(context.Background(), cfg, d))["notify"]; c.Status != Warn || !strings.Contains(c.Detail, "did not answer: org.freedesktop.Notifications: timeout") {
		t.Fatalf("a server that does not answer is a warning, not OK: %+v", c)
	}

	d.Notify = func(context.Context) notify.Probe { return notify.Probe{Backend: "none"} }
	checks := Run(context.Background(), cfg, d)
	if c := byName(checks)["notify"]; c.Status != Warn || c.Detail != "no desktop notification server; notifications disabled" {
		t.Fatalf("without a server notify only warns: %+v", c)
	}
	if Failed(checks) {
		t.Fatal("a missing notification server never fails doctor")
	}
}

func TestRunShouldRemindToAllowNotificationsOnMacOS(t *testing.T) {
	cfg, d := healthy(t)
	d.Notify = func(context.Context) notify.Probe {
		return notify.Probe{Backend: "macos", TerminalNotifier: "/opt/homebrew/bin/terminal-notifier"}
	}
	c := byName(Run(context.Background(), cfg, d))["notify"]
	if c.Status != OK || !strings.Contains(c.Detail, "terminal-notifier (/opt/homebrew/bin/terminal-notifier)") || !strings.Contains(c.Detail, "allow notifications for terminal-notifier in System Settings") {
		t.Fatalf("notify check = %+v", c)
	}

	d.Notify = func(context.Context) notify.Probe { return notify.Probe{Backend: "macos"} }
	c = byName(Run(context.Background(), cfg, d))["notify"]
	if c.Status != OK || !strings.Contains(c.Detail, "terminal-notifier not found") || !strings.Contains(c.Detail, "allow notifications for Script Editor in System Settings") {
		t.Fatalf("notify check = %+v", c)
	}
}

func TestRunShouldPassTheClaudeArgsAndEnvToVersionAndAuth(t *testing.T) {
	cfg, d := claudeHarness(t)
	cfg.Claude.Args = []string{"launch", "claude"}
	cfg.Claude.Env = map[string]string{"MIA_NO_RELOGIN": "1", "MIA_NO_ONBOARDING": "1"}
	var gotVersion, gotAuth, envVersion, envAuth []string
	d.Version = func(_ context.Context, b string, env []string, args ...string) (string, error) {
		if filepath.Base(b) == "claude" {
			gotVersion, envVersion = args, env
			return "2.1.303 (Claude Code)", nil
		}
		if len(args) != 0 || len(env) != 0 {
			t.Errorf("%s takes no launcher args nor env: %v %v", b, args, env)
		}
		return filepath.Base(b) + " 1.0", nil
	}
	d.ClaudeAuth = func(_ context.Context, _ string, env []string, args ...string) error {
		gotAuth, envAuth = args, env
		return nil
	}
	if results := Run(context.Background(), cfg, d); Failed(results) {
		t.Fatalf("healthy launcher setup must pass: %+v", results)
	}
	want := []string{"launch", "claude"}
	if !slices.Equal(gotVersion, want) || !slices.Equal(gotAuth, want) {
		t.Fatalf("claude.args must reach --version (%v) and auth status (%v)", gotVersion, gotAuth)
	}
	wantEnv := []string{"MIA_NO_ONBOARDING=1", "MIA_NO_RELOGIN=1"}
	if !slices.Equal(envVersion, wantEnv) || !slices.Equal(envAuth, wantEnv) {
		t.Fatalf("claude.env must reach --version (%v) and auth status (%v)", envVersion, envAuth)
	}
}

// hindsightHarness is a claude harness with the memory on and every hindsight piece in place;
// missing lists the paths Stat and ReadFile report as absent.
func hindsightHarness(t *testing.T, missing map[string]error) (config.Config, Deps) {
	cfg, d := claudeHarness(t)
	cfg.Claude.Hindsight = &config.Hindsight{Enabled: true, Config: "/home/u/.hindsight/coding-agent.json", Runtime: "/home/u/.hindsight/coding-agents"}
	d.Stat = func(p string) (fs.FileInfo, error) {
		if err := missing[p]; err != nil {
			return nil, err
		}
		return nil, nil
	}
	d.ReadFile = func(p string) ([]byte, error) {
		if err := missing[p]; err != nil {
			return nil, err
		}
		return []byte(`{"apiUrl":"https://mem.example","apiToken":"config-secret-marker"}`), nil
	}
	d.Getenv = func(k string) string {
		if k == "HINDSIGHT_API_TOKEN" {
			return "token-secret-marker"
		}
		return ""
	}
	return cfg, d
}

func TestDoctorShouldPassHindsightWhenEverythingIsInPlace(t *testing.T) {
	cfg, d := hindsightHarness(t, nil)
	results := Run(context.Background(), cfg, d)
	c, ok := byName(results)["hindsight"]
	if !ok || c.Status != OK || c.Detail != "/home/u/.hindsight/coding-agents" {
		t.Fatalf("hindsight must pass with the runtime path: %+v", c)
	}
	if Failed(results) {
		t.Fatalf("a complete memory setup must not fail doctor: %+v", results)
	}
}

func TestDoctorShouldSkipHindsightWhenDisabledOrOnOmp(t *testing.T) {
	enabled := &config.Hindsight{Enabled: true, Config: "/c.json", Runtime: "/r"}
	for name, setup := range map[string]func(*testing.T) (config.Config, Deps){
		"omp harness": func(t *testing.T) (config.Config, Deps) {
			cfg, d := healthy(t)
			cfg.Claude.Hindsight = enabled
			return cfg, d
		},
		"no block": func(t *testing.T) (config.Config, Deps) { return claudeHarness(t) },
		"disabled": func(t *testing.T) (config.Config, Deps) {
			cfg, d := claudeHarness(t)
			cfg.Claude.Hindsight = &config.Hindsight{Config: "/c.json", Runtime: "/r"}
			return cfg, d
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, d := setup(t)
			d.Stat = func(string) (fs.FileInfo, error) { t.Fatal("Stat must not run without the memory on"); return nil, nil }
			d.ReadFile = func(string) ([]byte, error) { t.Fatal("ReadFile must not run without the memory on"); return nil, nil }
			d.Getenv = func(string) string { t.Fatal("Getenv must not run without the memory on"); return "" }
			if c, ok := byName(Run(context.Background(), cfg, d))["hindsight"]; ok {
				t.Fatalf("no hindsight check expected: %+v", c)
			}
		})
	}
}

func TestDoctorShouldFailHindsightWhenTheConfigOrARuntimeScriptIsMissing(t *testing.T) {
	const conf, runtime = "/home/u/.hindsight/coding-agent.json", "/home/u/.hindsight/coding-agents"
	for name, err := range map[string]error{"absent": fs.ErrNotExist, "unreadable": fs.ErrPermission} {
		t.Run("config "+name, func(t *testing.T) {
			cfg, d := hindsightHarness(t, map[string]error{conf: err})
			results := Run(context.Background(), cfg, d)
			c := byName(results)["hindsight"]
			if c.Status != Fail || !strings.Contains(c.Detail, conf) || !strings.Contains(c.Detail, err.Error()) {
				t.Fatalf("a config the hooks cannot read must fail with its path and error: %+v", c)
			}
			if !Failed(results) {
				t.Fatal("doctor must fail")
			}
		})
	}
	for _, script := range []string{"mcp-server.js", "claude-hook.js", "claude-sessionstart-hook.js"} {
		t.Run(script, func(t *testing.T) {
			p := filepath.Join(runtime, "dist", script)
			cfg, d := hindsightHarness(t, map[string]error{p: fs.ErrNotExist})
			c := byName(Run(context.Background(), cfg, d))["hindsight"]
			if c.Status != Fail || !strings.Contains(c.Detail, p) || !strings.Contains(c.Detail, "npx @vectorize-io/hindsight-coding-agents install claude-code") {
				t.Fatalf("a missing runtime script must fail with its path and the install command: %+v", c)
			}
		})
	}
}

func TestDoctorShouldFailHindsightWithoutNode(t *testing.T) {
	cfg, d := hindsightHarness(t, nil)
	d.LookPath = func(b string) (string, error) {
		if b == "node" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + b, nil
	}
	if c := byName(Run(context.Background(), cfg, d))["hindsight"]; c.Status != Fail || c.Detail != "node not found in PATH" {
		t.Fatalf("the hooks run on node: %+v", c)
	}
}

func TestDoctorShouldWarnWithoutTheHindsightTokenAndNeverPrintIt(t *testing.T) {
	cfg, d := hindsightHarness(t, nil)
	d.Getenv = func(string) string { return "" }
	results := Run(context.Background(), cfg, d)
	if c := byName(results)["hindsight"]; c.Status != Warn || c.Detail != "HINDSIGHT_API_TOKEN not set (fine for a server without auth)" {
		t.Fatalf("a missing token only warns: %+v", c)
	}
	if Failed(results) {
		t.Fatalf("a warning must not fail doctor: %+v", results)
	}

	for name, missing := range map[string]map[string]error{
		"all in place":   nil,
		"script missing": {"/home/u/.hindsight/coding-agents/dist/claude-hook.js": fs.ErrNotExist},
	} {
		cfg, d := hindsightHarness(t, missing)
		for _, c := range Run(context.Background(), cfg, d) {
			if strings.Contains(c.Detail, "token-secret-marker") || strings.Contains(c.Detail, "config-secret-marker") || strings.Contains(c.Detail, "mem.example") {
				t.Fatalf("%s: %s leaks the token or the config content: %q", name, c.Name, c.Detail)
			}
		}
	}
}
