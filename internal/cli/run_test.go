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
