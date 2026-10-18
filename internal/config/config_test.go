package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadShouldReturnDefaultsWhenFileIsMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), noEnv)
	if err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	if cfg.Pipeline.MaxIterations != 3 || cfg.Pipeline.MaxParallel != 1 {
		t.Fatalf("iteration tolerance 3 and serial execution are the documented defaults, got %+v", cfg.Pipeline)
	}
	if cfg.Budget.Mode != BudgetTokens {
		t.Fatalf("default budget mode = %q, want tokens", cfg.Budget.Mode)
	}
	if strings.HasPrefix(cfg.Paths.WorktreesDir, "~") {
		t.Fatalf("~ must be expanded, got %q", cfg.Paths.WorktreesDir)
	}
}

func TestLoadShouldMergeFileOverDefaultsKeepingUnsetFields(t *testing.T) {
	p := writeFile(t, "pipeline:\n  max_iterations: 5\n  prompt_timeout: 10m\nomp:\n  model: openai/gpt-x\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.MaxIterations != 5 || cfg.Pipeline.PromptTimeout != 10*time.Minute {
		t.Fatalf("file values not applied: %+v", cfg.Pipeline)
	}
	if cfg.Pipeline.MaxParallel != 1 {
		t.Fatalf("unset field lost its default: max_parallel=%d", cfg.Pipeline.MaxParallel)
	}
	if cfg.Omp.Model != "openai/gpt-x" || cfg.Omp.ApprovalMode != "yolo" {
		t.Fatalf("omp merge wrong: %+v", cfg.Omp)
	}
}

func TestLoadShouldLetEnvOverrideFile(t *testing.T) {
	p := writeFile(t, "pipeline:\n  max_parallel: 2\n")
	cfg, err := Load(p, envOf(map[string]string{
		"DELAMAIN_MAX_PARALLEL":  "4",
		"DELAMAIN_HERDR_SESSION": "delamain-test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipeline.MaxParallel != 4 || cfg.Herdr.Session != "delamain-test" {
		t.Fatalf("env must win over file: %+v %+v", cfg.Pipeline, cfg.Herdr)
	}
}

func TestLoadShouldRejectNonIntegerEnvOverride(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "x.yaml"), envOf(map[string]string{"DELAMAIN_MAX_ITERATIONS": "three"}))
	if err == nil || !strings.Contains(err.Error(), "DELAMAIN_MAX_ITERATIONS") {
		t.Fatalf("want error naming the bad variable, got %v", err)
	}
}

func TestLoadShouldFailOnMalformedYAML(t *testing.T) {
	p := writeFile(t, "pipeline: [unclosed\n")
	if _, err := Load(p, noEnv); err == nil {
		t.Fatal("malformed YAML must fail loudly instead of silently using defaults")
	}
}

func TestValidateShouldRejectUnsafeOrMeaninglessValues(t *testing.T) {
	cases := map[string]func(*Config){
		"zero iterations":      func(c *Config) { c.Pipeline.MaxIterations = 0 },
		"zero parallel":        func(c *Config) { c.Pipeline.MaxParallel = 0 },
		"zero prompt timeout":  func(c *Config) { c.Pipeline.PromptTimeout = 0 },
		"unknown budget mode":  func(c *Config) { c.Budget.Mode = "credits" },
		"negative budget":      func(c *Config) { c.Budget.PerIssue = -1 },
		"soft ratio above one": func(c *Config) { c.Budget.SoftRatio = 1.5 },
		"soft ratio zero":      func(c *Config) { c.Budget.SoftRatio = 0 },
		"unknown approval":     func(c *Config) { c.Omp.ApprovalMode = "auto" },
		"empty model":          func(c *Config) { c.Omp.Model = " " },
		"unknown forge":        func(c *Config) { c.Forge.Kind = "bitbucket" },
		"empty label":          func(c *Config) { c.Forge.Labels.InProgress = "" },
		"poll too often":       func(c *Config) { c.Daemon.PollInterval = time.Second },
		"unknown red proof":    func(c *Config) { c.Gates.RedProof = "sometimes" },
		"label with comma":     func(c *Config) { c.Forge.Labels.Ready = "a,b" }, // glab splits on commas
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

func TestValidateShouldAcceptBoundaryValues(t *testing.T) {
	cfg := Default()
	cfg.Pipeline.MaxIterations = 1
	cfg.Budget.SoftRatio = 1
	cfg.Budget.PerIssue = 0 // disabled limit
	if err := cfg.Validate(); err != nil {
		t.Fatalf("boundary values must be valid: %v", err)
	}
}

func TestHerdrShouldBeOffUnlessTheFileOrEnvTurnsItOn(t *testing.T) {
	if cfg, err := Load(filepath.Join(t.TempDir(), "x.yaml"), noEnv); err != nil || cfg.Herdr.Enabled {
		t.Fatalf("the orchestrator's own flow is the default: %+v %v", cfg.Herdr, err)
	}
	cfg, err := Load(writeFile(t, "herdr:\n  enabled: true\n"), noEnv)
	if err != nil || !cfg.Herdr.Enabled {
		t.Fatalf("file: %+v %v", cfg.Herdr, err)
	}
	cfg, err = Load(writeFile(t, "herdr:\n  enabled: true\n"), envOf(map[string]string{"DELAMAIN_HERDR": "false"}))
	if err != nil || cfg.Herdr.Enabled {
		t.Fatalf("env wins over file: %+v %v", cfg.Herdr, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "x.yaml"), envOf(map[string]string{"DELAMAIN_HERDR": "talvez"})); err == nil || !strings.Contains(err.Error(), "DELAMAIN_HERDR") {
		t.Fatalf("want error naming the bad variable, got %v", err)
	}
}
