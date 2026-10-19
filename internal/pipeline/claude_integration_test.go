package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/prompts"
	embedded "github.com/paraizo/delamain/prompts"
)

// Contract test of the flags ClaudeAgents passes, against the real Claude Code and delamain's
// installed prompts. It spends a few thousand LLM tokens:
//
//	DELAMAIN_IT_CLAUDE_MODEL=haiku go test ./internal/pipeline/ -run Integration
func TestIntegrationClaudeAgentsShouldLoadDelamainsSubagentsAndSkills(t *testing.T) {
	model := os.Getenv("DELAMAIN_IT_CLAUDE_MODEL")
	if model == "" {
		t.Skip("set DELAMAIN_IT_CLAUDE_MODEL to run Claude Code integration tests")
	}
	root := t.TempDir()
	target := prompts.Target{Harness: prompts.HarnessClaude, RolesDir: filepath.Join(root, "roles"), ClaudeDir: filepath.Join(root, "claude")}
	set, err := prompts.Load(embedded.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.Install(target); err != nil {
		t.Fatal(err)
	}
	sessDir := filepath.Join(root, "sessions", "planner")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a := ClaudeAgents{Binary: "claude", Model: model, Effort: "low", PermissionMode: "bypassPermissions", AgentsFile: target.AgentsFile(), PluginDir: target.PluginDir()}
	ag, err := a.Start(context.Background(), AgentOptions{
		Name: "planner-r1", Dir: t.TempDir(), SessionDir: sessDir, SystemPrompt: target.RolePath("planner"),
		Env: gitx.AgentEnv(), LogPath: filepath.Join(root, "planner.log"), ErrPath: filepath.Join(root, "planner.stderr"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	turn, err := ag.Prompt(context.Background(), "Reply with exactly: ok", 2*time.Minute)
	if err != nil || turn.Failed() || turn.StopReason == "aborted" {
		stderr, _ := os.ReadFile(filepath.Join(root, "planner.stderr"))
		t.Fatalf("got %+v %v\nstderr: %s", turn, err, stderr)
	}
	streams, _ := filepath.Glob(filepath.Join(sessDir, "*.stream.jsonl"))
	if len(streams) != 1 {
		t.Fatalf("one transcript per session, got %v", streams)
	}
	transcript, _ := os.ReadFile(streams[0])
	for _, want := range []string{`"delamain-analyzer"`, `"delamain-security-reviewer"`, `delamain-tdd`, `delamain-commit-gate`} {
		if !strings.Contains(string(transcript), want) {
			t.Errorf("claude's init must list %s", want)
		}
	}
	got, err := a.Sessions(filepath.Join(root, "sessions"))
	if err != nil || len(got) != 1 || got[0].Role != "planner" || got[0].Usage.BudgetTokens() == 0 {
		t.Fatalf("the ledger reads the session's usage: %+v %v", got, err)
	}
}
