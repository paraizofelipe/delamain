// Package doctor verifies that the local toolchain can run the autonomous pipeline.
package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/paraizo/delamain/internal/config"
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
	Herdr  Herdr
	// Prompts is optional; when set, doctor lints it and checks the installation is current.
	Prompts       *prompts.Set
	PromptsTarget prompts.Target
}

// Run executes every check; it never stops at the first failure so the report is complete.
func Run(ctx context.Context, cfg config.Config, d Deps) []Check {
	var checks []Check
	add := func(name string, s Status, format string, a ...any) {
		checks = append(checks, Check{name, s, fmt.Sprintf(format, a...)})
	}

	found := map[string]string{}
	for _, bin := range []struct{ name, path string }{{"herdr", cfg.Herdr.Binary}, {"omp", cfg.Omp.Binary}, {"git", "git"}} {
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
		add(bin.name, OK, "%s (%s)", firstLine(v), p)
	}

	// The pipeline drives every agent over omp's RPC mode.
	if p, ok := found["omp"]; !ok {
		add("omp rpc", Fail, "omp unavailable")
	} else if err := d.OmpRPC(ctx, p); err != nil {
		add("omp rpc", Fail, "%v", err)
	} else {
		add("omp rpc", OK, "ready")
	}

	session := cfg.Herdr.Session
	label := session
	if label == "" {
		label = "default"
	}
	if sessions, err := d.Herdr.Sessions(ctx); err != nil {
		add("herdr server", Fail, "cannot list sessions: %v", err)
	} else {
		add("herdr server", Fail, "session %q not found", label)
		for _, s := range sessions {
			if (session == "" && s.Default) || s.Name == session {
				if s.Running {
					checks[len(checks)-1] = Check{"herdr server", OK, fmt.Sprintf("session %q running", s.Name)}
				} else {
					checks[len(checks)-1] = Check{"herdr server", Fail, fmt.Sprintf("session %q stopped (run: herdr --session %s server)", s.Name, s.Name)}
				}
			}
		}
	}

	forges := 0
	for _, bin := range []string{"gh", "glab"} {
		if _, err := d.LookPath(bin); err == nil {
			forges++
		}
	}
	if forges == 0 {
		add("forge cli", Warn, "neither gh nor glab found: delamain cannot push PRs/MRs (work stays local)")
	} else {
		add("forge cli", OK, "%d of gh/glab available", forges)
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
