// Package doctor verifies that the local toolchain can run the autonomous pipeline.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/herdr"
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

type Deps struct {
	LookPath func(string) (string, error)
	// Version runs "<bin> --version" and returns trimmed output.
	Version func(ctx context.Context, bin string) (string, error)
	// OmpRPC starts "<bin> --mode rpc" and returns once it is ready, or why it is not.
	OmpRPC func(ctx context.Context, bin string) error
	// ClaudeAuth runs "<bin> auth status" and says why claude is not logged in, if it is not.
	ClaudeAuth func(ctx context.Context, bin string) error
	Herdr      Herdr
	// Prompts is optional; when set, doctor lints it and checks the installation is current.
	Prompts       *prompts.Set
	PromptsTarget prompts.Target
	// ForgeRepos lists the repositories whose forge auth is checked.
	ForgeRepos func(ctx context.Context) ([]forge.Repo, error)
	// ForgeUser resolves repo's token and returns who it authenticates as and where it came from.
	ForgeUser func(ctx context.Context, repo forge.Repo) (login, source string, err error)
}

// Run executes every check; it never stops at the first failure so the report is complete.
func Run(ctx context.Context, cfg config.Config, d Deps) []Check {
	var checks []Check
	add := func(name string, s Status, format string, a ...any) {
		checks = append(checks, Check{name, s, fmt.Sprintf(format, a...)})
	}

	found := map[string]string{}
	harness := struct{ name, path string }{"omp", cfg.Omp.Binary}
	if cfg.Agent.Harness == config.HarnessClaude {
		harness.name, harness.path = "claude", cfg.Claude.Binary
	}
	bins := []struct{ name, path string }{harness, {"git", "git"}}
	if cfg.Herdr.Enabled {
		bins = append([]struct{ name, path string }{{"herdr", cfg.Herdr.Binary}}, bins...)
	} else {
		add("herdr", OK, "off: runs are followed with `delamain logs` (enable with herdr.enabled or --herdr)")
	}
	for _, bin := range bins {
		p, err := d.LookPath(bin.path)
		if err != nil {
			add(bin.name, Fail, "%s not found in PATH", bin.path)
			continue
		}
		v, err := d.Version(ctx, p)
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
		} else if err := d.ClaudeAuth(ctx, p); err != nil {
			add("claude auth", Fail, "%v (run: claude auth login)", err)
		} else {
			add("claude auth", OK, "logged in")
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

	if d.ForgeUser != nil {
		checks = append(checks, forgeAuth(ctx, d)...)
	}

	if _, err := d.LookPath("gitleaks"); err != nil {
		add("gitleaks", Warn, "not installed; secret-scan gate will be reported as skipped")
	} else {
		add("gitleaks", OK, "available")
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

// forgeAuth asks each repository's forge who its token belongs to (GET /user). Only a token the
// forge rejects fails: a missing token or an unreachable forge leaves local work possible.
func forgeAuth(ctx context.Context, d Deps) []Check {
	const name = "forge auth"
	repos, err := d.ForgeRepos(ctx)
	if err != nil {
		return []Check{{name, Warn, fmt.Sprintf("cannot list repositories: %v", err)}}
	}
	if len(repos) == 0 {
		return []Check{{name, Warn, "no repository with a forge remote to check"}}
	}
	var checks []Check
	for _, r := range repos {
		login, source, err := d.ForgeUser(ctx, r)
		var apiErr *forge.APIError
		switch {
		case err == nil:
			checks = append(checks, Check{name, OK, fmt.Sprintf("%s: %s on %s (token from %s)", r.Path, login, r.Host, source)})
		case errors.Is(err, forge.ErrTokenNotFound):
			checks = append(checks, Check{name, Warn, fmt.Sprintf("%s: no token for %s: %s; the token %s",
				r.Path, r.Host, forge.TokenHint(r.Kind, r.Host), forge.TokenPermissions(r.Kind))})
		case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden):
			checks = append(checks, Check{name, Fail, fmt.Sprintf("%s: token from %s rejected: %v", r.Path, source, err)})
		default:
			checks = append(checks, Check{name, Warn, fmt.Sprintf("%s: cannot reach %s: %v", r.Path, r.Host, err)})
		}
	}
	return checks
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
