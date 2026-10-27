package config

import (
	"os"
	"path/filepath"
	"slices"
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
			MROpened: "delamain:mr-opened", NeedsHuman: "delamain:needs-human", NeedsInfo: "delamain:needs-info", Review: "delamain:review"}},
		"prefix from file": {file: "forge:\n  labels:\n    prefix: paraizo\n", want: Labels{Prefix: "paraizo", Ready: "paraizo:ready",
			InProgress: "paraizo:in-progress", MROpened: "paraizo:mr-opened", NeedsHuman: "paraizo:needs-human", NeedsInfo: "paraizo:needs-info", Review: "paraizo:review"}},
		"an explicit label wins over the prefix": {file: "forge:\n  labels:\n    prefix: paraizo\n    in_progress: wip\n", want: Labels{Prefix: "paraizo",
			Ready: "paraizo:ready", InProgress: "wip", MROpened: "paraizo:mr-opened", NeedsHuman: "paraizo:needs-human", NeedsInfo: "paraizo:needs-info", Review: "paraizo:review"}},
		"env wins over file": {file: "forge:\n  labels:\n    prefix: paraizo\n", env: map[string]string{"DELAMAIN_LABEL_PREFIX": "bot"},
			want: Labels{Prefix: "bot", Ready: "bot:ready", InProgress: "bot:in-progress", MROpened: "bot:mr-opened", NeedsHuman: "bot:needs-human", NeedsInfo: "bot:needs-info", Review: "bot:review"}},
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
	file := "forge:\n  labels:\n    prefix: \"\"\n    ready: r\n    in_progress: i\n    mr_opened: m\n    needs_human: h\n    needs_info: n\n    review: v\n"
	if cfg, err := Load(writeFile(t, file), noEnv); err != nil || cfg.Forge.Labels.InProgress != "i" {
		t.Errorf("no prefix is fine when every label is set: %+v %v", cfg.Forge.Labels, err)
	}
}

func TestNotifyShouldDefaultToEnabledAutoWithoutStarted(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pr_opened", "needs_human", "halted", "needs_info", "budget", "recover"}
	if !cfg.Notify.Enabled || cfg.Notify.Backend != "auto" || !slices.Equal(cfg.Notify.Events, want) {
		t.Fatalf("notifications are on by default with backend auto and every event but started: %+v", cfg.Notify)
	}

	cfg, err = Load(writeFile(t, "notify:\n  backend: none\n"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notify.Enabled || cfg.Notify.Backend != "none" || !slices.Equal(cfg.Notify.Events, want) {
		t.Fatalf("a file setting only the backend keeps the other defaults: %+v", cfg.Notify)
	}
}

func TestNotifyShouldBeOverriddenByEnv(t *testing.T) {
	p := writeFile(t, "notify:\n  enabled: true\n  backend: dbus\n")
	cfg, err := Load(p, envOf(map[string]string{
		"DELAMAIN_NOTIFY_ENABLED": "false",
		"DELAMAIN_NOTIFY_BACKEND": "macos",
		"DELAMAIN_NOTIFY_EVENTS":  "halted, budget",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Enabled || cfg.Notify.Backend != "macos" || !slices.Equal(cfg.Notify.Events, []string{"halted", "budget"}) {
		t.Fatalf("env must override the file: %+v", cfg.Notify)
	}
	cfg, err = Load(p, envOf(map[string]string{"DELAMAIN_NOTIFY_EVENTS": " halted, ,budget,"}))
	if err != nil || !slices.Equal(cfg.Notify.Events, []string{"halted", "budget"}) {
		t.Fatalf("empty segments in DELAMAIN_NOTIFY_EVENTS are skipped: %v %v", cfg.Notify.Events, err)
	}
	if _, err := Load(p, envOf(map[string]string{"DELAMAIN_NOTIFY_ENABLED": "maybe"})); err == nil || !strings.Contains(err.Error(), "DELAMAIN_NOTIFY_ENABLED") {
		t.Fatalf("a non-boolean DELAMAIN_NOTIFY_ENABLED must be rejected, got %v", err)
	}
}

func TestNotifyShouldAcceptIterationEventButKeepItOffByDefault(t *testing.T) {
	cfg, err := Load(writeFile(t, "notify:\n  events: [iteration]\n"), noEnv)
	if err != nil {
		t.Fatalf("iteration is a known event: %v", err)
	}
	if !slices.Equal(cfg.Notify.Events, []string{"iteration"}) {
		t.Fatalf("events = %v", cfg.Notify.Events)
	}
	if slices.Contains(Default().Notify.Events, "iteration") {
		t.Fatalf("a run may raise up to 11 iteration toasts: it is off by default: %v", Default().Notify.Events)
	}
}

func TestValidateShouldRejectUnknownNotifyBackendOrEvent(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"backend", "notify:\n  backend: kde\n", "notify.backend"},
		{"event", "notify:\n  events: [pr_opened, deploy]\n", "notify.events"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tc.yaml), noEnv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error about %s, got %v", tc.want, err)
			}
		})
	}
	for _, b := range []string{"auto", "dbus", "macos", "none"} {
		cfg := Default()
		cfg.Notify.Backend = b
		cfg.Notify.Events = []string{"started", "iteration", "pr_opened", "needs_human", "halted", "needs_info", "budget", "recover"}
		if err := cfg.Validate(); err != nil {
			t.Errorf("backend %s with every known event is valid: %v", b, err)
		}
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

func TestPathsShouldPlaceMemoryDirUnderStateDir(t *testing.T) {
	p := Paths{StateDir: "/home/u/.local/state/delamain"}
	if got := p.MemoryDir(); got != "/home/u/.local/state/delamain/memory" {
		t.Fatalf("MemoryDir = %q", got)
	}
}

func TestReviewShouldDefaultToDisabledWithItsLimitsAndFixingOn(t *testing.T) {
	want := Review{Enabled: false, MaxComments: 15, PerReview: 500_000, Fix: true}
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review != want {
		t.Fatalf("review = %+v, want %+v", cfg.Review, want)
	}
	if Default().Review != want {
		t.Fatalf("Default().Review = %+v, want %+v", Default().Review, want)
	}
}

func TestLoadShouldReadTheReviewSectionFromTheFile(t *testing.T) {
	p := writeFile(t, "review:\n  enabled: true\n  max_comments: 7\n  per_review: 2.5\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := Review{Enabled: true, MaxComments: 7, PerReview: 2.5, Fix: true}
	if cfg.Review != want {
		t.Fatalf("review = %+v, want %+v (unset fix keeps its default)", cfg.Review, want)
	}
}

func TestReviewFixShouldBeTurnedOffByTheFile(t *testing.T) {
	p := writeFile(t, "review:\n  enabled: true\n  fix: false\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Fix {
		t.Fatal("review.fix: false in the file must win over the default")
	}
}

func TestReviewShouldBeOverriddenByEnv(t *testing.T) {
	p := writeFile(t, "review:\n  enabled: true\n  max_comments: 7\n  fix: true\n")
	cfg, err := Load(p, envOf(map[string]string{
		"DELAMAIN_REVIEW_ENABLED":      "false",
		"DELAMAIN_REVIEW_MAX_COMMENTS": "20",
		"DELAMAIN_REVIEW_PER_REVIEW":   "0.75",
		"DELAMAIN_REVIEW_FIX":          "false",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Review{Enabled: false, MaxComments: 20, PerReview: 0.75, Fix: false}
	if cfg.Review != want {
		t.Fatalf("env must win over file: review = %+v, want %+v", cfg.Review, want)
	}
}

func TestReviewShouldRejectMalformedEnvOverrides(t *testing.T) {
	for key, v := range map[string]string{
		"DELAMAIN_REVIEW_ENABLED":      "talvez",
		"DELAMAIN_REVIEW_FIX":          "nope",
		"DELAMAIN_REVIEW_MAX_COMMENTS": "many",
		"DELAMAIN_REVIEW_PER_REVIEW":   "half",
	} {
		if _, err := Load(filepath.Join(t.TempDir(), "x.yaml"), envOf(map[string]string{key: v})); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: want error naming the bad variable, got %v", key, v, err)
		}
	}
}

func TestValidateShouldRejectMeaninglessReviewLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Config)
		field  string
	}{
		"zero comments":     {func(c *Config) { c.Review.MaxComments = 0 }, "review.max_comments"},
		"negative comments": {func(c *Config) { c.Review.MaxComments = -2 }, "review.max_comments"},
		"zero budget":       {func(c *Config) { c.Review.PerReview = 0 }, "review.per_review"},
		"negative budget":   {func(c *Config) { c.Review.PerReview = -1 }, "review.per_review"},
		"enabled, zero cap": {func(c *Config) { c.Review.Enabled = true; c.Review.MaxComments = 0 }, "review.max_comments"},
	} {
		cfg := Default()
		tc.mutate(&cfg)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: want an error naming %s, got %v", name, tc.field, err)
		}
	}
	cfg := Default()
	cfg.Review.MaxComments, cfg.Review.PerReview = 1, 0.01
	if err := cfg.Validate(); err != nil {
		t.Fatalf("one comment and any positive budget are meaningful: %v", err)
	}
}

func TestReviewLabelShouldRejectAComma(t *testing.T) {
	_, err := Load(writeFile(t, "forge:\n  labels:\n    review: a,b\n"), noEnv)
	if err == nil || !strings.Contains(err.Error(), "forge.labels") || !strings.Contains(err.Error(), "a,b") {
		t.Fatalf("glab splits labels on commas: want a forge.labels error, got %v", err)
	}
}

func TestLoadShouldReadPerAgentModelsAndReasoning(t *testing.T) {
	p := writeFile(t, "agents:\n  implementer:\n    model: sonnet\n    effort: medium\n  analyzer:\n    model: opus\n    thinking: high\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["implementer"]; got != (AgentSettings{Model: "sonnet", Effort: "medium"}) {
		t.Fatalf("implementer: %+v", got)
	}
	if got := cfg.Agents["analyzer"]; got != (AgentSettings{Model: "opus", Thinking: "high"}) {
		t.Fatalf("analyzer: %+v", got)
	}
}

func TestAgentOverrideShouldPickTheActiveHarnessReasoningAndInheritWhenUnset(t *testing.T) {
	cfg := Default()
	cfg.Agents = map[string]AgentSettings{"implementer": {Model: "sonnet", Effort: "low", Thinking: "minimal"}}
	if m, r := cfg.AgentOverride("implementer"); m != "sonnet" || r != "minimal" {
		t.Fatalf("omp takes thinking: %q %q", m, r)
	}
	cfg.Agent.Harness = HarnessClaude
	if m, r := cfg.AgentOverride("implementer"); m != "sonnet" || r != "low" {
		t.Fatalf("claude takes effort: %q %q", m, r)
	}
	if m, r := cfg.AgentOverride("planner"); m != "" || r != "" {
		t.Fatalf("an agent without an entry inherits the harness defaults: %q %q", m, r)
	}
}

func TestValidateShouldRejectUnknownAgentsAndMeaninglessSettings(t *testing.T) {
	for name, agents := range map[string]map[string]AgentSettings{
		"unknown agent":    {"plannr": {Model: "opus"}},
		"blank model":      {"planner": {Model: "  "}},
		"invalid effort":   {"planner": {Effort: "huge"}},
		"invalid thinking": {"planner": {Thinking: "hihg"}},
	} {
		cfg := Default()
		cfg.Agents = agents
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: %+v must be rejected", name, agents)
		}
	}
	cfg := Default()
	cfg.Agents = map[string]AgentSettings{"review-verifier": {Model: "opus", Effort: "max"}, "planner": {}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("subagents and empty entries are valid: %v", err)
	}
}

func TestValidateShouldRejectAnOmpThinkingLevelOmpDoesNotTake(t *testing.T) {
	cfg := Default()
	cfg.Omp.Thinking = "hihg"
	if err := cfg.Validate(); err == nil {
		t.Fatal("omp.thinking must be one of omp's levels")
	}
	cfg.Omp.Thinking = "auto"
	cfg.Agents = map[string]AgentSettings{"implementer": {Thinking: "off"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("auto and off are omp levels: %v", err)
	}
}

func TestLoadShouldParseClaudeArgsEnvAndSettings(t *testing.T) {
	p := writeFile(t, "claude:\n  binary: mia\n  args: [launch, claude]\n  env:\n    MIA_NO_ONBOARDING: \"1\"\n  settings:\n    apiKeyHelper: mia hook api-key\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Claude
	if c.Binary != "mia" || len(c.Args) != 2 || c.Args[0] != "launch" || c.Args[1] != "claude" {
		t.Fatalf("binary and args: %+v", c)
	}
	if c.Env["MIA_NO_ONBOARDING"] != "1" || c.Settings["apiKeyHelper"] != "mia hook api-key" {
		t.Fatalf("env and settings: %+v", c)
	}
}

func TestValidateShouldRejectBlankClaudeArgsAndMalformedEnvKeys(t *testing.T) {
	for name, mutate := range map[string]func(*Claude){
		"blank arg":          func(c *Claude) { c.Args = []string{"launch", " "} },
		"empty env key":      func(c *Claude) { c.Env = map[string]string{"": "1"} },
		"env key with '='":   func(c *Claude) { c.Env = map[string]string{"A=B": "1"} },
		"env key with space": func(c *Claude) { c.Env = map[string]string{"A B": "1"} },
	} {
		cfg := Default()
		cfg.Agent.Harness = HarnessClaude
		mutate(&cfg.Claude)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s must be rejected: %+v", name, cfg.Claude)
		}
	}
	cfg := Default()
	cfg.Agent.Harness = HarnessClaude
	cfg.Claude.Args = []string{"launch", "claude"}
	cfg.Claude.Env = map[string]string{"MIA_NO_UPDATE_CHECK": "1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a launcher with args and env is valid: %v", err)
	}
}

func TestValidateShouldRefuseAttributionInClaudeSettings(t *testing.T) {
	cfg := Default()
	cfg.Agent.Harness = HarnessClaude
	cfg.Claude.Settings = map[string]any{"attribution": map[string]any{"commit": "me"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("attribution is delamain policy and must be refused")
	}
	cfg.Claude.Settings = map[string]any{"apiKeyHelper": "mia hook api-key"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("other settings keys are valid: %v", err)
	}
}

func TestWithProjectOverridesShouldPreferProjectFieldsAndInheritTheRest(t *testing.T) {
	cfg := Default()
	cfg.Claude.Args = []string{"launch", "claude"}
	cfg.Claude.Env = map[string]string{"MIA_NO_ONBOARDING": "1"}

	merged, changed, err := cfg.WithProjectOverrides(Agent{Harness: HarnessClaude}, Claude{Binary: "mia"}, Omp{}, nil)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if merged.Agent.Harness != HarnessClaude || merged.Claude.Binary != "mia" {
		t.Fatalf("project fields must win: %+v", merged.Claude)
	}
	if merged.Claude.Model != "opus" || len(merged.Claude.Args) != 2 {
		t.Fatalf("absent fields inherit the global ones: %+v", merged.Claude)
	}

	merged, changed, err = cfg.WithProjectOverrides(Agent{}, Claude{Binary: "claude", Args: []string{}, Env: map[string]string{}}, Omp{}, nil)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(merged.Claude.Args) != 0 || len(merged.Claude.Env) != 0 || merged.Claude.Binary != "claude" {
		t.Fatalf("an explicitly empty list or map clears the global one: %+v", merged.Claude)
	}

	if _, changed, err := cfg.WithProjectOverrides(Agent{}, Claude{}, Omp{}, nil); changed || err != nil {
		t.Fatalf("a project without overrides keeps the global config: changed=%v err=%v", changed, err)
	}
}

func TestWithProjectOverridesShouldMergeAgentsByName(t *testing.T) {
	cfg := Default()
	cfg.Agents = map[string]AgentSettings{"planner": {Model: "opus"}, "implementer": {Model: "sonnet", Effort: "high"}}
	merged, changed, err := cfg.WithProjectOverrides(Agent{}, Claude{}, Omp{}, map[string]AgentSettings{"implementer": {Model: "haiku"}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if merged.Agents["implementer"] != (AgentSettings{Model: "haiku"}) {
		t.Fatalf("the project entry replaces the global one whole: %+v", merged.Agents["implementer"])
	}
	if merged.Agents["planner"].Model != "opus" || len(cfg.Agents["implementer"].Effort) == 0 {
		t.Fatalf("other names and the global config stay untouched: %+v / %+v", merged.Agents, cfg.Agents)
	}
}

func TestWithProjectOverridesShouldRejectAnInvalidMerge(t *testing.T) {
	cfg := Default()
	cfg.Agent.Harness = HarnessClaude
	if _, _, err := cfg.WithProjectOverrides(Agent{}, Claude{Effort: "huge"}, Omp{}, nil); err == nil {
		t.Fatal("an invalid project effort must fail before any agent starts")
	}
	if _, _, err := cfg.WithProjectOverrides(Agent{Harness: "cursor"}, Claude{}, Omp{}, nil); err == nil {
		t.Fatal("an unknown harness must be rejected")
	}
}

func TestLoadShouldDefaultHindsightPathsWhenTheBlockIsPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for name, body := range map[string]string{
		"empty block":  "claude:\n  hindsight: {}\n",
		"only enabled": "claude:\n  hindsight:\n    enabled: true\n",
		"disabled":     "claude:\n  hindsight:\n    enabled: false\n",
	} {
		cfg, err := Load(writeFile(t, body), noEnv)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		h := cfg.Claude.Hindsight
		if h == nil {
			t.Fatalf("%s: a present block must be loaded", name)
		}
		if h.Config != filepath.Join(home, ".hindsight", "coding-agent.json") || h.Runtime != filepath.Join(home, ".hindsight", "coding-agents") {
			t.Fatalf("%s: empty paths default under ~/.hindsight, expanded: %+v", name, h)
		}
	}
}

func TestLoadShouldExpandTildeInHindsightPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := writeFile(t, "agent:\n  harness: claude\nclaude:\n  hindsight:\n    enabled: true\n    config: ~/mem/agent.json\n    runtime: /opt/hindsight\n")
	cfg, err := Load(p, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Claude.Hindsight
	if h == nil || !h.Enabled {
		t.Fatalf("enabled block must be loaded: %+v", h)
	}
	if h.Config != filepath.Join(home, "mem", "agent.json") || h.Runtime != "/opt/hindsight" {
		t.Fatalf("~ is expanded and absolute paths are kept: %+v", h)
	}
}

func TestLoadShouldLeaveHindsightNilWhenTheBlockIsAbsent(t *testing.T) {
	cfg, err := Load(writeFile(t, "claude:\n  model: sonnet\n"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Claude.Hindsight != nil {
		t.Fatalf("no block means no memory config, got %+v", cfg.Claude.Hindsight)
	}
	if Default().Claude.Hindsight != nil {
		t.Fatal("the defaults must not turn the memory on")
	}
}

func TestValidateShouldRequireHindsightPathsWhenEnabled(t *testing.T) {
	for name, h := range map[string]Hindsight{
		"blank config":  {Enabled: true, Config: " ", Runtime: "/opt/hindsight"},
		"blank runtime": {Enabled: true, Config: "/etc/agent.json", Runtime: ""},
	} {
		cfg := Default()
		cfg.Agent.Harness = HarnessClaude
		cfg.Claude.Hindsight = &h
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "claude.hindsight.") || !strings.Contains(err.Error(), "is required when enabled") {
			t.Errorf("%s must be rejected while enabled, got %v", name, err)
		}
	}
	cfg := Default()
	cfg.Agent.Harness = HarnessClaude
	cfg.Claude.Hindsight = &Hindsight{Enabled: false}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a disabled block needs no paths: %v", err)
	}
	cfg.Claude.Hindsight = &Hindsight{Enabled: true, Config: "/etc/agent.json", Runtime: "/opt/hindsight"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("an enabled block with both paths is valid: %v", err)
	}
}
