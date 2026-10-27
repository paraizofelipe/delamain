// Package doctor verifies that the local toolchain can run the autonomous pipeline.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/herdr"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/prompts"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

type Check struct {
	Name   string
	Status Status
	Detail string
}

// Herdr is the subset of the herdr client doctor needs.
type Herdr interface {
	Sessions(ctx context.Context) ([]herdr.Session, error)
}

// ForgeIdentity is who a repository's forge token authenticates as. It never holds the token.
type ForgeIdentity struct {
	Kind   forge.Kind
	Host   string
	Login  string
	Source string // where the token came from, e.g. "env GH_TOKEN"
}

type Deps struct {
	LookPath func(string) (string, error)
	// Version runs "<bin> [args...] --version" with env appended and returns trimmed output.
	Version func(ctx context.Context, bin string, env []string, args ...string) (string, error)
	// OmpRPC starts "<bin> --mode rpc" and returns once it is ready, or why it is not.
	OmpRPC func(ctx context.Context, bin string) error
	// ClaudeAuth runs "<bin> [args...] auth status" and says why claude is not logged in, if
	// it is not. args and env carry a launcher's subcommand and variables (claude.args/env).
	ClaudeAuth func(ctx context.Context, bin string, env []string, args ...string) error
	Herdr      Herdr
	// ForgeAuth resolves the forge token of the repository at dir and asks the forge who it
	// belongs to. A repository without forge returns a zero identity and no error.
	ForgeAuth func(ctx context.Context, dir string) (ForgeIdentity, error)
	// Prompts is optional; when set, doctor lints it and checks the installation is current.
	Prompts       *prompts.Set
	PromptsTarget prompts.Target
	// Notify probes the desktop notification backend; nil skips the check.
	Notify func(ctx context.Context) notify.Probe
	// Stat, ReadFile and Getenv back the hindsight check, which only calls them when
	// claude.hindsight is enabled on the claude harness.
	Stat     func(string) (fs.FileInfo, error)
	ReadFile func(string) ([]byte, error)
	Getenv   func(string) string
}

// Run executes every check; it never stops at the first failure so the report is complete.
func Run(ctx context.Context, cfg config.Config, d Deps) []Check {
	var checks []Check
	add := func(name string, s Status, format string, a ...any) {
		checks = append(checks, Check{name, s, fmt.Sprintf(format, a...)})
	}

	found := map[string]string{}
	harness := struct {
		name, path string
		args, env  []string
	}{"omp", cfg.Omp.Binary, nil, nil}
	if cfg.Agent.Harness == config.HarnessClaude {
		harness.name, harness.path = "claude", cfg.Claude.Binary
		harness.args, harness.env = cfg.Claude.Args, cfg.Claude.Environ()
	}
	type bin = struct {
		name, path string
		args, env  []string
	}
	bins := []bin{harness, {"git", "git", nil, nil}}
	if cfg.Herdr.Enabled {
		bins = append([]bin{{"herdr", cfg.Herdr.Binary, nil, nil}}, bins...)
	} else {
		add("herdr", OK, "off: runs are followed with `delamain logs` (enable with herdr.enabled or --herdr)")
	}
	for _, bin := range bins {
		p, err := d.LookPath(bin.path)
		if err != nil {
			add(bin.name, Fail, "%s not found in PATH", bin.path)
			continue
		}
		v, err := d.Version(ctx, p, bin.env, bin.args...)
		if err != nil {
			add(bin.name, Fail, "%s --version failed: %v", p, err)
			continue
		}
		found[bin.name] = p
		if bin.name == "claude" && olderThan(v, MinClaude) {
			add(bin.name, Warn, "%s (%s): older than %s, whose stream-json delamain relies on", firstLine(v), p, MinClaude)
			continue
		}
		add(bin.name, OK, "%s (%s)", firstLine(v), p)
	}

	if cfg.Agent.Harness == config.HarnessClaude {
		// Without credentials every agent fails its first turn.
		if p, ok := found["claude"]; !ok {
			add("claude auth", Fail, "claude unavailable")
		} else if err := d.ClaudeAuth(ctx, p, cfg.Claude.Environ(), cfg.Claude.Args...); err != nil {
			add("claude auth", Fail, "%v (run: claude auth login)", err)
		} else {
			add("claude auth", OK, "logged in")
		}
		if h := cfg.Claude.Hindsight; h != nil && h.Enabled {
			checks = append(checks, hindsight(*h, d))
		}
	} else if p, ok := found["omp"]; !ok { // the pipeline drives every agent over omp's RPC mode
		add("omp rpc", Fail, "omp unavailable")
	} else if err := d.OmpRPC(ctx, p); err != nil {
		add("omp rpc", Fail, "%v", err)
	} else {
		add("omp rpc", OK, "ready")
	}

	if cfg.Herdr.Enabled {
		checks = append(checks, herdrServer(ctx, cfg, d.Herdr)...)
	}

	repos := cfg.Daemon.Repos
	if len(repos) == 0 {
		repos = []string{"."}
	}
	for _, dir := range repos {
		checks = append(checks, forgeAuth(ctx, d, dir))
	}

	if _, err := d.LookPath("gitleaks"); err != nil {
		add("gitleaks", Warn, "not installed; secret-scan gate will be reported as skipped")
	} else {
		add("gitleaks", OK, "available")
	}

	if d.Notify != nil {
		checks = append(checks, notifyCheck(d.Notify(ctx)))
	}

	if d.Prompts != nil {
		checks = append(checks, promptChecks(d.Prompts, d.PromptsTarget)...)
	}

	if err := writable(cfg.Paths.StateDir); err != nil {
		add("state dir", Fail, "%s: %v", cfg.Paths.StateDir, err)
	} else {
		add("state dir", OK, "%s", cfg.Paths.StateDir)
	}
	return checks
}

// forgeAuth checks dir's forge token against the API. A missing token or one the forge rejects
// is a failure: run and daemon stop on that repository until a token is provided.
func forgeAuth(ctx context.Context, d Deps, dir string) Check {
	where := ""
	if dir != "." {
		where = dir + ": "
	}
	id, err := d.ForgeAuth(ctx, dir)
	var apiErr *forge.APIError
	switch {
	case err == nil && id.Host == "":
		return Check{"forge auth", OK, where + "local (no forge)"}
	case err == nil:
		return Check{"forge auth", OK, fmt.Sprintf("%s%s on %s (token from %s)", where, id.Login, id.Host, id.Source)}
	case errors.Is(err, forge.ErrTokenNotFound):
		return Check{"forge auth", Fail, fmt.Sprintf("%s%v: runs on this repository fail until a token is provided; %s", where, err, forge.TokenPermissions(id.Kind))}
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
		return Check{"forge auth", Fail, fmt.Sprintf("%s%v (token from %s); %s", where, err, id.Source, forge.TokenPermissions(id.Kind))}
	default:
		return Check{"forge auth", Warn, fmt.Sprintf("%s%v", where, err)}
	}
}

// hindsight checks what the agents' memory needs to run and reports only the first problem.
// The config is read to prove it is readable; its content (URL, token) is discarded unseen.
func hindsight(h config.Hindsight, d Deps) Check {
	if _, err := d.ReadFile(h.Config); err != nil {
		return Check{"hindsight", Fail, fmt.Sprintf("%s: %v", h.Config, err)}
	}
	for _, script := range []string{"mcp-server.js", "claude-hook.js", "claude-sessionstart-hook.js"} {
		p := filepath.Join(h.Runtime, "dist", script)
		if _, err := d.Stat(p); err != nil {
			return Check{"hindsight", Fail, fmt.Sprintf("%s: %v (run: npx @vectorize-io/hindsight-coding-agents install claude-code)", p, err)}
		}
	}
	if _, err := d.LookPath("node"); err != nil {
		return Check{"hindsight", Fail, "node not found in PATH"}
	}
	if d.Getenv("HINDSIGHT_API_TOKEN") == "" { // only emptiness is checked: the value never reaches a Detail
		return Check{"hindsight", Warn, "HINDSIGHT_API_TOKEN not set (fine for a server without auth)"}
	}
	return Check{"hindsight", OK, h.Runtime}
}

// notifyCheck reports the notification backend; without one it only warns: runs never need it.
func notifyCheck(p notify.Probe) Check {
	switch p.Backend {
	case config.NotifyDBus:
		if p.Err != nil {
			return Check{"notify", Warn, fmt.Sprintf("dbus: notification server did not answer: %v", p.Err)}
		}
		actions := "actions supported"
		if !p.Actions {
			actions = "no actions (clicks disabled)"
		}
		return Check{"notify", OK, fmt.Sprintf("dbus: %s %s, %s", p.Server, p.Version, actions)}
	case config.NotifyMacOS:
		if p.TerminalNotifier != "" {
			return Check{"notify", OK, fmt.Sprintf("macos: terminal-notifier (%s); allow notifications for terminal-notifier in System Settings > Notifications", p.TerminalNotifier)}
		}
		return Check{"notify", OK, "macos: osascript (terminal-notifier not found: no logo, no click); allow notifications for Script Editor in System Settings > Notifications"}
	}
	return Check{"notify", Warn, "no desktop notification server; notifications disabled"}
}

func promptChecks(set *prompts.Set, t prompts.Target) []Check {
	if errs := set.Lint(); len(errs) > 0 {
		return []Check{{"prompts", Fail, fmt.Sprintf("%d lint problems, first: %v (run: delamain prompts lint)", len(errs), errs[0])}}
	}
	missing, outdated, err := set.Drift(t)
	switch {
	case err != nil:
		return []Check{{"prompts", Fail, err.Error()}}
	case len(missing) > 0:
		return []Check{{"prompts", Fail, fmt.Sprintf("%d files not installed, e.g. %s (run: delamain prompts install)", len(missing), missing[0])}}
	case len(outdated) > 0:
		return []Check{{"prompts", Warn, fmt.Sprintf("%d installed files differ from source, e.g. %s (run: delamain prompts install)", len(outdated), outdated[0])}}
	}
	return []Check{{"prompts", OK, "valid and installed"}}
}

// Failed reports whether any check failed (warnings do not fail).
func Failed(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func writable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(filepath.Clean(name))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// herdrServer checks that the configured herdr session is running, when herdr shows the runs.
func herdrServer(ctx context.Context, cfg config.Config, h Herdr) []Check {
	session := cfg.Herdr.Session
	label := session
	if label == "" {
		label = "default"
	}
	sessions, err := h.Sessions(ctx)
	if err != nil {
		return []Check{{"herdr server", Fail, fmt.Sprintf("cannot list sessions: %v", err)}}
	}
	for _, s := range sessions {
		if (session == "" && s.Default) || s.Name == session {
			if s.Running {
				return []Check{{"herdr server", OK, fmt.Sprintf("session %q running", s.Name)}}
			}
			return []Check{{"herdr server", Fail, fmt.Sprintf("session %q stopped (run: herdr --session %s server)", s.Name, s.Name)}}
		}
	}
	return []Check{{"herdr server", Fail, fmt.Sprintf("session %q not found", label)}}
}

// MinClaude is the oldest Claude Code whose stream-json ends a cut-off turn properly.
const MinClaude = "2.1.290"

// olderThan compares the leading x.y.z of a --version output with min, ignoring a pre-release
// suffix; unparsable is not older.
func olderThan(version, minimum string) bool {
	parse := func(s string) []int {
		f := strings.Fields(s)
		if len(f) == 0 {
			return nil
		}
		xyz, _, _ := strings.Cut(f[0], "-")
		var n []int
		for _, p := range strings.Split(xyz, ".") {
			v, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			n = append(n, v)
		}
		return n
	}
	got, want := parse(version), parse(minimum)
	if len(got) != 3 || len(want) != 3 {
		return false
	}
	return slices.Compare(got, want) < 0
}
