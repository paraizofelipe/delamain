package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/herdr"
)

type fakeHerdr struct {
	sessions    []herdr.Session
	sessionsErr error
	integration string
	intErr      error
}

func (f fakeHerdr) Sessions(context.Context) ([]herdr.Session, error) {
	return f.sessions, f.sessionsErr
}
func (f fakeHerdr) IntegrationStatus(context.Context, string) (string, error) {
	return f.integration, f.intErr
}

func healthy(t *testing.T) (config.Config, Deps) {
	cfg := config.Default()
	cfg.Paths.StateDir = t.TempDir()
	return cfg, Deps{
		LookPath: func(b string) (string, error) { return "/usr/bin/" + b, nil },
		Version:  func(_ context.Context, b string) (string, error) { return filepath.Base(b) + " 1.0\nextra", nil },
		Herdr: fakeHerdr{
			sessions:    []herdr.Session{{Name: "default", Default: true, Running: true}},
			integration: "current",
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

func TestRunShouldFailWhenOmpIntegrationIsMissing(t *testing.T) {
	cfg, d := healthy(t)
	d.Herdr = fakeHerdr{sessions: d.Herdr.(fakeHerdr).sessions, integration: "not installed"}
	c := byName(Run(context.Background(), cfg, d))["herdr omp integration"]
	if c.Status != Fail {
		t.Fatalf("screen-detection fallback produced false idle states; must fail: %+v", c)
	}
}

func TestRunShouldFailWhenConfiguredSessionIsStoppedOrAbsent(t *testing.T) {
	cfg, d := healthy(t)
	cfg.Herdr.Session = "delamain-test"
	d.Herdr = fakeHerdr{integration: "current", sessions: []herdr.Session{{Name: "delamain-test", Running: false}}}
	if c := byName(Run(context.Background(), cfg, d))["herdr server"]; c.Status != Fail {
		t.Fatalf("stopped session: %+v", c)
	}
	cfg.Herdr.Session = "ghost"
	if c := byName(Run(context.Background(), cfg, d))["herdr server"]; c.Status != Fail {
		t.Fatalf("absent session: %+v", c)
	}
}

func TestRunShouldReportEveryProblemInsteadOfStoppingAtFirst(t *testing.T) {
	cfg, d := healthy(t)
	d.LookPath = func(b string) (string, error) { return "", errors.New("not found") }
	d.Herdr = fakeHerdr{sessionsErr: errors.New("socket down"), intErr: errors.New("socket down")}
	m := byName(Run(context.Background(), cfg, d))
	for _, name := range []string{"herdr", "omp", "git", "herdr server", "herdr omp integration"} {
		if m[name].Status != Fail {
			t.Errorf("%s should fail: %+v", name, m[name])
		}
	}
	if m["forge cli"].Status != Warn || m["gitleaks"].Status != Warn {
		t.Errorf("optional tools are warnings, not failures: %+v %+v", m["forge cli"], m["gitleaks"])
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
