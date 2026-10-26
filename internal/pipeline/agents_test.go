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

func TestRPCAgentsShouldLetTheRoleOverrideTheModelAndThinking(t *testing.T) {
	a := RPCAgents{Binary: "omp", Model: "openai-codex/gpt-5.6-terra", Thinking: "medium", ApprovalMode: "yolo"}
	got := strings.Join(a.args(AgentOptions{Model: "openai-codex/gpt-5.6-mini", Reasoning: "low"}), " ")
	if !strings.HasPrefix(got, "--model openai-codex/gpt-5.6-mini --thinking low ") {
		t.Fatalf("omp args must carry the role's model and thinking: %s", got)
	}
}

func TestClaudeAgentsShouldLetTheRoleOverrideTheModelAndEffort(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions"}
	if got := strings.Join(a.args("sid", "implement", AgentOptions{Model: "sonnet", Reasoning: "medium"}), " "); !strings.HasPrefix(got, "--model sonnet --effort medium ") {
		t.Fatalf("claude args must carry the role's model and effort: %s", got)
	}
	if got := strings.Join(a.args("sid", "implement", AgentOptions{Model: "sonnet"}), " "); !strings.HasPrefix(got, "--model sonnet --effort high ") {
		t.Fatalf("an unset effort inherits claude.effort: %s", got)
	}
}

func TestClaudeAgentsShouldRunUnattendedApartFromTheUsersClaudeSetup(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions", AgentsFile: "/p/agents.json", PluginDir: "/p/plugin"}
	got := strings.Join(a.args("sid", "be the planner", AgentOptions{}), " ")
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

func TestClaudeAgentsShouldMergeConfiguredSettingsUnderTheAttributionPolicy(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions",
		Settings: map[string]any{"apiKeyHelper": "/usr/local/bin/mia hook api-key", "attribution": "mine"}}
	got := strings.Join(a.args("sid", "plan", AgentOptions{}), " ")
	want := `--settings {"apiKeyHelper":"/usr/local/bin/mia hook api-key","attribution":{"commit":"","pr":"","sessionUrl":false}}`
	if !strings.Contains(got, want) {
		t.Fatalf("the configured settings merge in and attribution stays delamain's: %s", got)
	}
}

func TestClaudeAgentsShouldKeepTheAttributionPolicyWhenSettingsDoNotEncode(t *testing.T) {
	a := ClaudeAgents{Settings: map[string]any{"broken": func() {}}} // Validate() never lets this in
	got := strings.Join(a.args("sid", "plan", AgentOptions{}), " ")
	want := `--settings {"attribution":{"commit":"","pr":"","sessionUrl":false}}`
	if !strings.Contains(got, want) {
		t.Fatalf("no Co-Authored-By may come back through a settings map that fails to encode: %s", got)
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
	args := a.args("sid", "review the PR", AgentOptions{Confine: &Confinement{Checkout: "/wt/review-pr-5-1", Base: "b4se", Artifact: "/run/5/review.json"}})
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
	// --output=<path> (write any file). The diff runs no external diff nor textconv: the PR's
	// .gitattributes would pick which of the operator's drivers runs on its files.
	want := []string{
		"Read(//wt/review-pr-5-1/**)",
		"Bash(git -C /wt/review-pr-5-1 diff --no-ext-diff --no-textconv b4se...HEAD)", "Bash(git -C /wt/review-pr-5-1 log b4se..HEAD --oneline)",
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
	if plain := strings.Join(a.args("sid", "plan", AgentOptions{}), " "); !strings.Contains(plain, "--permission-mode bypassPermissions") || strings.Contains(plain, "--allowedTools") {
		t.Fatalf("issue runs keep the configured mode: %s", plain)
	}
}

// flagValues collects the values that follow every occurrence of flag in args.
func flagValues(args []string, flag string) map[string]bool {
	values := map[string]bool{}
	for i, arg := range args {
		if arg == flag {
			for _, v := range args[i+1:] {
				if strings.HasPrefix(v, "--") {
					break
				}
				values[v] = true
			}
		}
	}
	return values
}

func TestClaudeAgentsShouldLetAFixingSessionEditAndCommitOnlyInItsCheckout(t *testing.T) {
	a := ClaudeAgents{Model: "opus", Effort: "high", PermissionMode: "bypassPermissions", AgentsFile: "/p/agents.json", PluginDir: "/p/plugin"}
	args := a.args("sid", "fix the PR", AgentOptions{Confine: &Confinement{Checkout: "/wt/feat/x", Base: "b4se", Artifact: "/run/5/responses.json", Fix: true, CommitMessage: "/run/5/commit-msg.txt"}})
	got := strings.Join(args, " ")
	if !strings.Contains(got, "--permission-mode dontAsk") || !strings.Contains(got, "--tools Read,Grep,Glob,Bash,Edit,Write,Agent ") {
		t.Fatalf("a fixing session edits files and still denies everything not allowed: %s", got)
	}
	// Status, add and commit in the checkout only, every one an exact command: the message comes
	// from a file the session writes, so no argument is left open (a prefix rule would also take
	// --pathspec-from-file=<any file>, -F <any file> or a second -m). Hooks are off because a
	// hooksPath inside the checkout would be editable. No test, build, push or forge command:
	// running code it edits would be a free shell.
	want := []string{
		"Read(//wt/feat/x/**)",
		"Bash(git -C /wt/feat/x diff --no-ext-diff --no-textconv b4se...HEAD)", "Bash(git -C /wt/feat/x log b4se..HEAD --oneline)",
		"Edit(//run/5/responses.json)",
		"Edit(//wt/feat/x/**)",
		"Bash(git -C /wt/feat/x status --porcelain)", "Bash(git -C /wt/feat/x add -A)",
		"Edit(//run/5/commit-msg.txt)", "Bash(git -C /wt/feat/x -c core.hooksPath=/dev/null commit -F /run/5/commit-msg.txt)",
	}
	allowed := flagValues(args, "--allowedTools")
	if len(allowed) != len(want) {
		t.Fatalf("allowed rules %v, want exactly %v", allowed, want)
	}
	for _, rule := range want {
		if !allowed[rule] {
			t.Errorf("missing allow rule %q in %v", rule, allowed)
		}
	}
	for rule := range allowed {
		if strings.Contains(rule, ":*") || strings.Contains(rule, "Bash(") && strings.Contains(rule, "*") {
			t.Errorf("open-ended rule %q lets the agent add any argument", rule)
		}
	}
	denied := flagValues(args, "--disallowed-tools")
	for _, rule := range []string{"AskUserQuestion", "Edit(//wt/feat/x/.git)", "Edit(//wt/feat/x/.git/**)"} {
		if !denied[rule] {
			t.Errorf("a fixing session must deny %q, denied %v", rule, denied)
		}
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
