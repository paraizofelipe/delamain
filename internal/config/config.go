// Package config loads delamain settings from a YAML file with environment overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// BudgetMode selects what the budget limits measure.
type BudgetMode string

const (
	// BudgetTokens counts input+output+cacheWrite tokens (cacheRead excluded, same as omp /goal).
	BudgetTokens BudgetMode = "tokens"
	// BudgetUSD counts provider-reported cost in US dollars.
	BudgetUSD BudgetMode = "usd"
)

type Config struct {
	Agent    Agent    `yaml:"agent"`
	Herdr    Herdr    `yaml:"herdr"`
	Omp      Omp      `yaml:"omp"`
	Claude   Claude   `yaml:"claude"`
	Pipeline Pipeline `yaml:"pipeline"`
	Budget   Budget   `yaml:"budget"`
	Forge    Forge    `yaml:"forge"`
	Gates    Gates    `yaml:"gates"`
	Daemon   Daemon   `yaml:"daemon"`
	Paths    Paths    `yaml:"paths"`
	Notify   Notify   `yaml:"notify"`
}

// Herdr optionally shows runs in herdr workspaces. Off by default: the orchestrator drives
// agents and gates itself, and `delamain logs` follows them.
type Herdr struct {
	Enabled bool   `yaml:"enabled"`
	Binary  string `yaml:"binary"`
	// Session targets a named herdr session; empty means the default session.
	Session string `yaml:"session"`
}

// Harness is the coding agent CLI that runs the planner, implementer and reviewer.
type Harness string

const (
	HarnessOmp    Harness = "omp"
	HarnessClaude Harness = "claude"
)

type Agent struct {
	// Harness picks the section (omp or claude) the agents are started from.
	Harness Harness `yaml:"harness"`
}

// Claude runs the agents as Claude Code in headless stream-json mode (`claude -p`).
type Claude struct {
	Binary string `yaml:"binary"`
	Model  string `yaml:"model"`
	// Effort is Claude Code's --effort: low|medium|high|xhigh|max.
	Effort string `yaml:"effort"`
	// PermissionMode is bypassPermissions (omp's yolo) or acceptEdits.
	PermissionMode string `yaml:"permission_mode"`
}

type Omp struct {
	Binary string `yaml:"binary"`
	// AgentDir is omp's agent directory, where delamain installs its agents and skills.
	AgentDir     string `yaml:"agent_dir"`
	Model        string `yaml:"model"`
	Thinking     string `yaml:"thinking"`
	ApprovalMode string `yaml:"approval_mode"`
}

type Pipeline struct {
	MaxIterations int           `yaml:"max_iterations"`
	MaxParallel   int           `yaml:"max_parallel"`
	PromptTimeout time.Duration `yaml:"prompt_timeout"`
	// CheckTimeout bounds each deterministic gate (lint, typecheck, test) run by the orchestrator.
	CheckTimeout time.Duration `yaml:"check_timeout"`
}

type Budget struct {
	Mode BudgetMode `yaml:"mode"`
	// PerIssue and Daily are token counts or USD depending on Mode. Zero disables the limit.
	PerIssue  float64 `yaml:"per_issue"`
	Daily     float64 `yaml:"daily"`
	SoftRatio float64 `yaml:"soft_ratio"`
}

// Forge kinds. ForgeAuto detects GitHub/GitLab from the origin URL; ForgeNone delivers locally only.
const (
	ForgeAuto   = "auto"
	ForgeGitHub = "github"
	ForgeGitLab = "gitlab"
	ForgeNone   = "none"
)

type Forge struct {
	Kind   string `yaml:"kind"`
	Labels Labels `yaml:"labels"`
}

// Labels drive the issue lifecycle: ready → in_progress → mr_opened | needs_human | needs_info.
// A label left unset is named <prefix>:<stage>, e.g. paraizo:in-progress.
type Labels struct {
	Prefix     string `yaml:"prefix"`
	Ready      string `yaml:"ready"`
	InProgress string `yaml:"in_progress"`
	MROpened   string `yaml:"mr_opened"`
	NeedsHuman string `yaml:"needs_human"`
	NeedsInfo  string `yaml:"needs_info"`
}

// Gates are the orchestrator's own checks; a repository's .delamain.yaml may override them.
type Gates struct {
	// RedProof says when new tests must fail on the base branch: bug | always | never.
	RedProof string `yaml:"red_proof"`
	// RequireTests rejects diffs that change production code without touching a test.
	RequireTests bool `yaml:"require_tests"`
}

// Daemon configures `delamain daemon`: which repositories to poll for ready issues, and how often.
type Daemon struct {
	Repos        []string      `yaml:"repos"`
	PollInterval time.Duration `yaml:"poll_interval"`
}

// Notify configures desktop notifications (D-Bus on Linux, Notification Center on macOS).
type Notify struct {
	Enabled bool `yaml:"enabled"`
	// Backend is auto | dbus | macos | none.
	Backend string `yaml:"backend"`
	// Events lists the events that raise a toast.
	Events []string `yaml:"events"`
}

type Paths struct {
	StateDir string `yaml:"state_dir"`
	// WorktreesDir holds every run's worktree, as <worktrees_dir>/<repo>/<branch>; `delamain
	// status` lists the worktrees found there.
	WorktreesDir string `yaml:"worktrees_dir"`
	// PromptsDir overrides the embedded prompts when set.
	PromptsDir string `yaml:"prompts_dir"`
}

// RolesDir holds installed role bodies appended to the agents' system prompt.
func (p Paths) RolesDir() string { return filepath.Join(p.StateDir, "prompts", "roles") }

// ClaudeDir holds the subagents (agents.json) and skills plugin installed for the claude harness.
func (p Paths) ClaudeDir() string { return filepath.Join(p.StateDir, "prompts", "claude") }

// DefaultLabelPrefix names the lifecycle labels when forge.labels.prefix is unset.
const DefaultLabelPrefix = "delamain"

// withPrefix names each unset label after Prefix, keeping the ones set explicitly.
func (l Labels) withPrefix() Labels {
	for dst, stage := range map[*string]string{
		&l.Ready: "ready", &l.InProgress: "in-progress", &l.MROpened: "mr-opened",
		&l.NeedsHuman: "needs-human", &l.NeedsInfo: "needs-info",
	} {
		if *dst == "" && l.Prefix != "" {
			*dst = l.Prefix + ":" + stage
		}
	}
	return l
}

// Default returns the configuration used when no file or env override is present.
func Default() Config {
	cfg := defaults()
	cfg.Forge.Labels = cfg.Forge.Labels.withPrefix()
	return cfg
}

// defaults leaves the label names unset, so a prefix from the file or env still names them.
func defaults() Config {
	return Config{
		Agent: Agent{Harness: HarnessOmp},
		Herdr: Herdr{Binary: "herdr"},
		Omp: Omp{
			Binary:       "omp",
			AgentDir:     "~/.omp/agent",
			Model:        "openai-codex/gpt-5.6-terra",
			Thinking:     "medium",
			ApprovalMode: "yolo",
		},
		Claude:   Claude{Binary: "claude", Model: "opus", Effort: "high", PermissionMode: "bypassPermissions"},
		Pipeline: Pipeline{MaxIterations: 3, MaxParallel: 1, PromptTimeout: 30 * time.Minute, CheckTimeout: 20 * time.Minute},
		Budget:   Budget{Mode: BudgetTokens, PerIssue: 2_000_000, Daily: 10_000_000, SoftRatio: 0.8},
		Forge:    Forge{Kind: ForgeAuto, Labels: Labels{Prefix: DefaultLabelPrefix}},
		Gates:    Gates{RedProof: "bug", RequireTests: true},
		Daemon:   Daemon{PollInterval: 5 * time.Minute},
		Paths: Paths{
			StateDir:     "~/.local/state/delamain",
			WorktreesDir: "~/projects/.worktrees",
		},
	}
}

// DefaultPath is ~/.config/delamain/config.yaml, honoring XDG_CONFIG_HOME.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(base, "delamain", "config.yaml")
}

// Load reads path (a missing file is not an error), applies env overrides, expands ~ and validates.
func Load(path string, getenv func(string) string) (Config, error) {
	cfg := defaults()
	data, err := os.ReadFile(path) //nolint:gosec // reading the user-chosen config path is the intent
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	cfg.Forge.Labels = cfg.Forge.Labels.withPrefix()
	cfg.Paths.StateDir = expandHome(cfg.Paths.StateDir)
	cfg.Paths.WorktreesDir = expandHome(cfg.Paths.WorktreesDir)
	cfg.Paths.PromptsDir = expandHome(cfg.Paths.PromptsDir)
	cfg.Omp.AgentDir = expandHome(cfg.Omp.AgentDir)
	for i, r := range cfg.Daemon.Repos {
		cfg.Daemon.Repos[i] = expandHome(r)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	strs := map[string]*string{
		"DELAMAIN_AGENT_HARNESS": (*string)(&cfg.Agent.Harness),
		"DELAMAIN_CLAUDE_MODEL":  &cfg.Claude.Model,
		"DELAMAIN_HERDR_SESSION": &cfg.Herdr.Session,
		"DELAMAIN_OMP_MODEL":     &cfg.Omp.Model,
		"DELAMAIN_OMP_AGENT_DIR": &cfg.Omp.AgentDir,
		"DELAMAIN_STATE_DIR":     &cfg.Paths.StateDir,
		"DELAMAIN_PROMPTS_DIR":   &cfg.Paths.PromptsDir,
		"DELAMAIN_LABEL_PREFIX":  &cfg.Forge.Labels.Prefix,
	}
	for key, dst := range strs {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	if v := getenv("DELAMAIN_HERDR"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DELAMAIN_HERDR: %q is not a boolean", v)
		}
		cfg.Herdr.Enabled = b
	}
	ints := map[string]*int{
		"DELAMAIN_MAX_ITERATIONS": &cfg.Pipeline.MaxIterations,
		"DELAMAIN_MAX_PARALLEL":   &cfg.Pipeline.MaxParallel,
	}
	for key, dst := range ints {
		v := getenv(key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %q is not an integer", key, v)
		}
		*dst = n
	}
	return nil
}

// Validate rejects settings that would make the pipeline unsafe or meaningless.
func (c Config) Validate() error {
	var errs []error
	if c.Pipeline.MaxIterations < 1 {
		errs = append(errs, errors.New("pipeline.max_iterations must be >= 1"))
	}
	if c.Pipeline.MaxParallel < 1 {
		errs = append(errs, errors.New("pipeline.max_parallel must be >= 1"))
	}
	if c.Pipeline.PromptTimeout <= 0 {
		errs = append(errs, errors.New("pipeline.prompt_timeout must be > 0"))
	}
	if c.Pipeline.CheckTimeout <= 0 {
		errs = append(errs, errors.New("pipeline.check_timeout must be > 0"))
	}
	if c.Budget.Mode != BudgetTokens && c.Budget.Mode != BudgetUSD {
		errs = append(errs, fmt.Errorf("budget.mode must be %q or %q, got %q", BudgetTokens, BudgetUSD, c.Budget.Mode))
	}
	if c.Budget.PerIssue < 0 || c.Budget.Daily < 0 {
		errs = append(errs, errors.New("budget limits must be >= 0"))
	}
	if c.Budget.SoftRatio <= 0 || c.Budget.SoftRatio > 1 {
		errs = append(errs, errors.New("budget.soft_ratio must be in (0, 1]"))
	}
	switch c.Agent.Harness {
	case HarnessOmp:
		errs = append(errs, c.Omp.validate()...)
	case HarnessClaude:
		errs = append(errs, c.Claude.validate()...)
	default:
		errs = append(errs, fmt.Errorf("agent.harness %q is not one of omp|claude", c.Agent.Harness))
	}
	switch c.Forge.Kind {
	case ForgeAuto, ForgeGitHub, ForgeGitLab, ForgeNone:
	default:
		errs = append(errs, fmt.Errorf("forge.kind %q is not one of auto|github|gitlab|none", c.Forge.Kind))
	}
	l := c.Forge.Labels
	if strings.Contains(l.Prefix, ",") || strings.HasSuffix(l.Prefix, ":") {
		errs = append(errs, fmt.Errorf("forge.labels.prefix %q must have no comma or trailing colon", l.Prefix))
	}
	for _, v := range []string{l.Ready, l.InProgress, l.MROpened, l.NeedsHuman, l.NeedsInfo} {
		if strings.TrimSpace(v) == "" || strings.Contains(v, ",") {
			errs = append(errs, fmt.Errorf("forge.labels: %q must be non-empty and contain no comma", v))
		}
	}
	if c.Daemon.PollInterval < 30*time.Second {
		errs = append(errs, errors.New("daemon.poll_interval must be >= 30s (forge API rate limits)"))
	}
	switch c.Gates.RedProof {
	case "bug", "always", "never":
	default:
		errs = append(errs, fmt.Errorf("gates.red_proof %q is not one of bug|always|never", c.Gates.RedProof))
	}
	return errors.Join(errs...)
}

func (o Omp) validate() []error {
	var errs []error
	switch o.ApprovalMode {
	case "always-ask", "write", "yolo":
	default:
		errs = append(errs, fmt.Errorf("omp.approval_mode %q is not one of always-ask|write|yolo", o.ApprovalMode))
	}
	if strings.TrimSpace(o.Model) == "" {
		errs = append(errs, errors.New("omp.model is required"))
	}
	return errs
}

func (c Claude) validate() []error {
	var errs []error
	switch c.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		errs = append(errs, fmt.Errorf("claude.effort %q is not one of low|medium|high|xhigh|max", c.Effort))
	}
	switch c.PermissionMode {
	case "bypassPermissions", "acceptEdits":
	default:
		errs = append(errs, fmt.Errorf("claude.permission_mode %q is not one of bypassPermissions|acceptEdits", c.PermissionMode))
	}
	if strings.TrimSpace(c.Model) == "" {
		errs = append(errs, errors.New("claude.model is required"))
	}
	return errs
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}
