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
		"zero iterations":             func(c *Config) { c.Pipeline.MaxIterations = 0 },
		"zero parallel":               func(c *Config) { c.Pipeline.MaxParallel = 0 },
		"zero prompt timeout":         func(c *Config) { c.Pipeline.PromptTimeout = 0 },
		"unknown budget mode":         func(c *Config) { c.Budget.Mode = "credits" },
		"negative budget":             func(c *Config) { c.Budget.PerIssue = -1 },
		"soft ratio above one":        func(c *Config) { c.Budget.SoftRatio = 1.5 },
		"soft ratio zero":             func(c *Config) { c.Budget.SoftRatio = 0 },
		"unknown approval":            func(c *Config) { c.Omp.ApprovalMode = "auto" },
		"empty model":                 func(c *Config) { c.Omp.Model = " " },
		"unknown forge":               func(c *Config) { c.Forge.Kind = "bitbucket" },
		"empty label":                 func(c *Config) { c.Forge.Labels.InProgress = "" },
		"poll too often":              func(c *Config) { c.Daemon.PollInterval = time.Second },
		"unknown red proof":           func(c *Config) { c.Gates.RedProof = "sometimes" },
		"label with comma":            func(c *Config) { c.Forge.Labels.Ready = "a,b" }, // glab splits on commas
		"unknown harness":             func(c *Config) { c.Agent.Harness = "codex" },
		"empty claude model":          func(c *Config) { c.Agent.Harness = HarnessClaude; c.Claude.Model = " " },
		"unknown effort":              func(c *Config) { c.Agent.Harness = HarnessClaude; c.Claude.Effort = "huge" },
		"negative profile iterations": func(c *Config) { c.Complexity.High.MaxIterations = -1 },
		"negative profile budget":     func(c *Config) { c.Complexity.Low.PerIssue = -1 },
		"unknown permission":          func(c *Config) { c.Agent.Harness = HarnessClaude; c.Claude.PermissionMode = "plan" },
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

func TestHarnessShouldDefaultToOmpAndBeChosenByFileOrEnv(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "x.yaml"), noEnv)
	if err != nil || cfg.Agent.Harness != HarnessOmp {
		t.Fatalf("omp stays the default harness: %q %v", cfg.Agent.Harness, err)
	}
	if cfg.Claude.Binary != "claude" || cfg.Claude.Model != "opus" || cfg.Claude.Effort != "high" || cfg.Claude.PermissionMode != "bypassPermissions" {
		t.Fatalf("claude defaults: %+v", cfg.Claude)
	}
	cfg, err = Load(writeFile(t, "agent:\n  harness: claude\nclaude:\n  model: sonnet\n"), noEnv)
	if err != nil || cfg.Agent.Harness != HarnessClaude || cfg.Claude.Model != "sonnet" || cfg.Claude.Effort != "high" {
		t.Fatalf("file: %+v %+v %v", cfg.Agent, cfg.Claude, err)
	}
	env := envOf(map[string]string{"DELAMAIN_AGENT_HARNESS": "omp", "DELAMAIN_CLAUDE_MODEL": "haiku"})
	cfg, err = Load(writeFile(t, "agent:\n  harness: claude\n"), env)
	if err != nil || cfg.Agent.Harness != HarnessOmp || cfg.Claude.Model != "haiku" {
		t.Fatalf("env wins over file: %+v %+v %v", cfg.Agent, cfg.Claude, err)
	}
}

func TestValidateShouldOnlyCheckTheSelectedHarnessSection(t *testing.T) {
	cfg := Default()
	cfg.Agent.Harness = HarnessClaude
	cfg.Omp.Model, cfg.Omp.ApprovalMode = "", "auto"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an unused omp section must not block claude: %v", err)
	}
	cfg = Default()
	cfg.Claude.Model, cfg.Claude.Effort = "", "huge"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an unused claude section must not block omp: %v", err)
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

func TestLabelsShouldBeNamedAfterThePrefix(t *testing.T) {
	cases := map[string]struct {
		file string
		env  map[string]string
		want Labels
	}{
		"default prefix": {want: Labels{Prefix: "delamain", Ready: "delamain:ready", InProgress: "delamain:in-progress",
			MROpened: "delamain:mr-opened", NeedsHuman: "delamain:needs-human", NeedsInfo: "delamain:needs-info"}},
		"prefix from file": {file: "forge:\n  labels:\n    prefix: paraizo\n", want: Labels{Prefix: "paraizo", Ready: "paraizo:ready",
			InProgress: "paraizo:in-progress", MROpened: "paraizo:mr-opened", NeedsHuman: "paraizo:needs-human", NeedsInfo: "paraizo:needs-info"}},
		"an explicit label wins over the prefix": {file: "forge:\n  labels:\n    prefix: paraizo\n    in_progress: wip\n", want: Labels{Prefix: "paraizo",
			Ready: "paraizo:ready", InProgress: "wip", MROpened: "paraizo:mr-opened", NeedsHuman: "paraizo:needs-human", NeedsInfo: "paraizo:needs-info"}},
		"env wins over file": {file: "forge:\n  labels:\n    prefix: paraizo\n", env: map[string]string{"DELAMAIN_LABEL_PREFIX": "bot"},
			want: Labels{Prefix: "bot", Ready: "bot:ready", InProgress: "bot:in-progress", MROpened: "bot:mr-opened", NeedsHuman: "bot:needs-human", NeedsInfo: "bot:needs-info"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(writeFile(t, tc.file), envOf(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Forge.Labels != tc.want {
				t.Fatalf("labels = %+v, want %+v", cfg.Forge.Labels, tc.want)
			}
		})
	}
}

func TestLabelsShouldRejectAPrefixThatCannotNameThem(t *testing.T) {
	for name, file := range map[string]string{
		"empty prefix, labels unset": "forge:\n  labels:\n    prefix: \"\"\n",
		"comma":                      "forge:\n  labels:\n    prefix: a,b\n",
		"trailing colon":             "forge:\n  labels:\n    prefix: \"paraizo:\"\n",
	} {
		if _, err := Load(writeFile(t, file), noEnv); err == nil || !strings.Contains(err.Error(), "forge.labels") {
			t.Errorf("%s: want a forge.labels error, got %v", name, err)
		}
	}
	file := "forge:\n  labels:\n    prefix: \"\"\n    ready: r\n    in_progress: i\n    mr_opened: m\n    needs_human: h\n    needs_info: n\n"
	if cfg, err := Load(writeFile(t, file), noEnv); err != nil || cfg.Forge.Labels.InProgress != "i" {
		t.Errorf("no prefix is fine when every label is set: %+v %v", cfg.Forge.Labels, err)
	}
}

func TestLoadShouldReadComplexityProfiles(t *testing.T) {
	p := writeFile(t, "complexity:\n  high:\n    max_iterations: 5\n    per_issue: 4000000\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	it, budget := cfg.Limits("high")
	if it != 5 || budget != 4e6 {
		t.Fatalf("Limits(high) = (%d, %v), want (5, 4e6)", it, budget)
	}
}

func TestLimitsShouldInheritGlobalForZeroOrUnknown(t *testing.T) {
	cfg := Default()
	for _, level := range []string{"low", ""} {
		it, budget := cfg.Limits(level)
		if it != 3 || budget != 2e6 {
			t.Fatalf("Limits(%q) = (%d, %v), want (3, 2e6)", level, it, budget)
		}
	}
	cfg.Complexity.High = Profile{MaxIterations: 5}
	it, budget := cfg.Limits("high")
	if it != 5 || budget != 2e6 {
		t.Fatalf("Limits(high) = (%d, %v), want (5, 2e6)", it, budget)
	}
}
