package cli

import (
	"slices"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/prompts"
	embedded "github.com/paraizo/delamain/prompts"
)

func TestAgentNamesShouldListEveryRoleAndSubagentOfTheManifest(t *testing.T) {
	set, err := prompts.Load(embedded.FS)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Concat(set.Manifest.Roles, set.Manifest.Agents)
	if got := config.AgentNames; !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("config.AgentNames = %v, the manifest has %v", got, want)
	}
}

func TestPromptsTargetShouldCarryTheSubagentsModelsOfTheActiveHarness(t *testing.T) {
	cfg := config.Default()
	cfg.Agent.Harness = config.HarnessClaude
	cfg.Agents = map[string]config.AgentSettings{"analyzer": {Model: "opus", Effort: "high", Thinking: "low"}, "planner": {Thinking: "low"}}
	got := promptsTarget(cfg).Agents
	if len(got) != 1 || got["analyzer"] != (prompts.AgentModel{Model: "opus", Reasoning: "high"}) {
		t.Fatalf("only overrides that apply to claude reach the install: %+v", got)
	}
}
