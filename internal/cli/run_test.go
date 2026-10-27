package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/prompts"
)

func TestAgentsForShouldCarryTheClaudeLauncherArgsEnvAndSettings(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Harness = config.HarnessClaude
	cfg.Claude.Args = []string{"launch", "claude"}
	cfg.Claude.Env = map[string]string{"MIA_NO_UPDATE_CHECK": "1", "MIA_NO_ONBOARDING": "1"}
	cfg.Claude.Settings = map[string]any{"apiKeyHelper": "mia hook api-key"}
	a, ok := agentsFor(cfg, prompts.Target{}).(pipeline.ClaudeAgents)
	if !ok {
		t.Fatalf("harness claude must start ClaudeAgents: %T", agentsFor(cfg, prompts.Target{}))
	}
	if !slices.Equal(a.BinaryArgs, []string{"launch", "claude"}) {
		t.Fatalf("launcher args: %v", a.BinaryArgs)
	}
	if !slices.Equal(a.Env, []string{"MIA_NO_ONBOARDING=1", "MIA_NO_UPDATE_CHECK=1"}) {
		t.Fatalf("env must be rendered sorted as K=V: %v", a.Env)
	}
	if a.Settings["apiKeyHelper"] != "mia hook api-key" {
		t.Fatalf("settings: %v", a.Settings)
	}
}

func TestAgentsForShouldCarryTheHindsightConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Harness = config.HarnessClaude
	cfg.Claude.Hindsight = &config.Hindsight{Enabled: true, Config: "/h/coding-agent.json", Runtime: "/h/coding-agents"}
	a, ok := agentsFor(cfg, prompts.Target{}).(pipeline.ClaudeAgents)
	if !ok {
		t.Fatalf("harness claude must start ClaudeAgents: %T", agentsFor(cfg, prompts.Target{}))
	}
	if a.Hindsight == nil || *a.Hindsight != *cfg.Claude.Hindsight {
		t.Fatalf("the sessions must know the hindsight runtime: %+v", a.Hindsight)
	}
}

func TestProjectConfigShouldRetargetTheHarnessPerRepo(t *testing.T) {
	cfg := config.Default() // harness omp
	repo := t.TempDir()
	if _, changed, err := projectConfig(cfg, repo); changed || err != nil {
		t.Fatalf("a repo without .delamain.yaml keeps the global config: changed=%v err=%v", changed, err)
	}

	body := "agent:\n  harness: claude\nclaude:\n  binary: mia\n  args: [launch, claude]\n  settings:\n    apiKeyHelper: mia hook api-key\nagents:\n  implementer:\n    model: haiku\n"
	if err := os.WriteFile(filepath.Join(repo, ".delamain.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, changed, err := projectConfig(cfg, repo)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	a, ok := agentsFor(merged, promptsTarget(merged)).(pipeline.ClaudeAgents)
	if !ok || a.Binary != "mia" || !slices.Equal(a.BinaryArgs, []string{"launch", "claude"}) {
		t.Fatalf("the project picks claude via its launcher: %+v", a)
	}
	if merged.Agents["implementer"].Model != "haiku" {
		t.Fatalf("per-agent overrides come from the project too: %+v", merged.Agents)
	}

	if err := os.WriteFile(filepath.Join(repo, ".delamain.yaml"), []byte("claude:\n  effort: huge\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agent.Harness = config.HarnessClaude
	if _, _, err := projectConfig(cfg, repo); err == nil || !strings.Contains(err.Error(), ".delamain.yaml") {
		t.Fatalf("an invalid project override must name the file: %v", err)
	}
}

func TestPromptsTargetShouldIsolateTheClaudeDirPerAgentOverrides(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Harness = config.HarnessClaude
	base := promptsTarget(cfg).ClaudeDir

	cfg.Agents = map[string]config.AgentSettings{"implementer": {Model: "haiku"}}
	over := promptsTarget(cfg).ClaudeDir
	if filepath.Dir(over) != base {
		t.Fatalf("overrides must install under the shared dir, in their own: %s vs %s", over, base)
	}
	if again := promptsTarget(cfg).ClaudeDir; again != over {
		t.Fatalf("equal overrides must share one install: %s vs %s", again, over)
	}
	cfg.Agents = map[string]config.AgentSettings{"implementer": {Model: "sonnet"}}
	if other := promptsTarget(cfg).ClaudeDir; other == over || filepath.Dir(other) != base {
		t.Fatalf("a different override set writes its own agents.json: %s vs %s", other, over)
	}

	cfg.Agent.Harness = config.HarnessOmp
	if omp := promptsTarget(cfg).ClaudeDir; omp != base {
		t.Fatalf("omp discovers its own agent dir, nothing may move: %s", omp)
	}
}
