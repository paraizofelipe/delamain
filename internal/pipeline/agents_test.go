package pipeline

import (
	"context"
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
	got := strings.Join(a.args("sid", "be the planner", nil), " ")
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

func TestClaudeAgentsShouldConfineASessionToReadingItsCheckout(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions", AgentsFile: "/p/agents.json", PluginDir: "/p/plugin"}
	args := a.args("sid", "review the PR", &Confinement{Checkout: "/wt/review-pr-5-1", Base: "b4se", Artifact: "/run/5/review.json"})
	got := strings.Join(args, " ")
	if strings.Contains(got, "bypassPermissions") || !strings.Contains(got, "--permission-mode dontAsk") {
		t.Fatalf("a confined session denies everything not allowed below: %s", got)
	}
	if !strings.Contains(got, "--tools Read,Grep,Glob,Bash,Write,Agent ") {
		t.Fatalf("no LSP, web or edit tools for a confined session: %s", got)
	}
	allowed := map[string]bool{}
	for i, arg := range args {
		if arg == "--allowedTools" {
			for _, rule := range args[i+1:] {
				if strings.HasPrefix(rule, "--") {
					break
				}
				allowed[rule] = true
			}
		}
	}
	// Exact commands only: a prefix rule ("diff:*") would allow --no-index (read any file) and
	// --output=<path> (write any file).
	want := []string{
		"Read(//wt/review-pr-5-1/**)",
		"Bash(git -C /wt/review-pr-5-1 diff b4se...HEAD)", "Bash(git -C /wt/review-pr-5-1 log b4se..HEAD --oneline)",
		"Edit(//run/5/review.json)",
	}
	if len(allowed) != len(want) {
		t.Fatalf("allowed rules %v, want exactly %v", allowed, want)
	}
	for _, rule := range want {
		if !allowed[rule] {
			t.Errorf("missing allow rule %q in %v", rule, allowed)
		}
	}
	for rule := range allowed {
		if strings.Contains(rule, ":*") {
			t.Errorf("open-ended rule %q lets the agent add any flag", rule)
		}
	}
	for _, kept := range []string{"--setting-sources project --strict-mcp-config", "--permission-prompts none"} {
		if !strings.Contains(got, kept) {
			t.Errorf("a confined session keeps %q: %s", kept, got)
		}
	}
	if plain := strings.Join(a.args("sid", "plan", nil), " "); !strings.Contains(plain, "--permission-mode bypassPermissions") || strings.Contains(plain, "--allowedTools") {
		t.Fatalf("issue runs keep the configured mode: %s", plain)
	}
}

func TestRPCAgentsShouldRefuseAConfinedSession(t *testing.T) {
	dir := t.TempDir()
	a := RPCAgents{Binary: filepath.Join(dir, "omp-not-installed"), Model: "m", Thinking: "medium", ApprovalMode: "yolo"}
	_, err := a.Start(context.Background(), AgentOptions{
		SessionDir: dir, SystemPrompt: filepath.Join(dir, "r.md"), LogPath: filepath.Join(dir, "a.log"), ErrPath: filepath.Join(dir, "a.err"),
		Confine: &Confinement{Checkout: "/wt", Base: "b", Artifact: "/a.json"},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot confine") {
		t.Fatalf("omp has no per-command allowlist: a confined session must be refused, not started with a free shell: %v", err)
	}
}
