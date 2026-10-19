package pipeline

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/claude"
	"github.com/paraizo/delamain/internal/store"
)

func TestRPCAgentsShouldPassTheOmpConfigSessionDirAndRoleToOmp(t *testing.T) {
	a := RPCAgents{Binary: "omp", Model: "openai-codex/gpt-5.6-terra", Thinking: "medium", ApprovalMode: "yolo"}
	got := strings.Join(a.args(AgentOptions{SessionDir: "/run/sessions/planner", SystemPrompt: "/roles/planner.md"}), " ")
	want := "--model openai-codex/gpt-5.6-terra --thinking medium --approval-mode yolo --session-dir /run/sessions/planner --append-system-prompt /roles/planner.md"
	if got != want {
		t.Fatalf("omp args:\n got %s\nwant %s", got, want)
	}
}

func TestClaudeAgentsShouldRunUnattendedApartFromTheUsersClaudeSetup(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions", AgentsFile: "/p/agents.json", PluginDir: "/p/plugin"}
	got := strings.Join(a.args("sid", "be the planner"), " ")
	for _, want := range []string{
		"--model opus --effort high --permission-mode bypassPermissions",
		"--session-id sid --no-session-persistence",
		"--append-system-prompt be the planner",
		"--agents /p/agents.json --plugin-dir /p/plugin",
		"--setting-sources project --strict-mcp-config",
		"--permission-prompts none --disallowed-tools AskUserQuestion",
		`--settings {"attribution":{"commit":"","pr":"","sessionUrl":false}}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claude args lack %q: %s", want, got)
		}
	}
}

func TestClaudeAgentsSessionsShouldReadEachRolesUsageFile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("planner/a"+claude.UsageSuffix, `{"input":10,"output":2,"cacheRead":5,"cacheWrite":1,"cost":{"total":0.5}}`)
	write("planner/a.stream.jsonl", `{"type":"result"}`)
	got, err := ClaudeAgents{}.Sessions(dir)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if s := got[0]; s.Role != "planner" || s.Usage != (store.Usage{Input: 10, Output: 2, CacheRead: 5, CacheWrite: 1, CostUSD: 0.5}) {
		t.Fatalf("%+v", s)
	}
	write("reviewer/b"+claude.UsageSuffix, `not json`)
	if _, err := (ClaudeAgents{}).Sessions(dir); err == nil {
		t.Fatal("an unreadable usage file must fail the accounting, not count as zero")
	}
}

func TestNewSessionIDShouldBeARandomUUID(t *testing.T) {
	a, err := newSessionID()
	b, _ := newSessionID()
	if err != nil || a == b || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(a) {
		t.Fatalf("%q %q %v", a, b, err)
	}
}
