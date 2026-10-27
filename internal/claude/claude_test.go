package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/agent"
)

// The test binary doubles as a fake claude: with CLAUDE_FAKE set it speaks stream-json and
// plays the scenario named by the variable. CLAUDE_FAKE_TRACE receives the env and every
// stdin line.
func TestMain(m *testing.M) {
	if s := os.Getenv("CLAUDE_FAKE"); s != "" {
		fakeClaude(s)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func assistantEvent(id string, input int64, content ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{
		"id": id, "model": "claude-test", "content": content,
		"usage": map[string]any{"input_tokens": input, "output_tokens": 2, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 7},
	}}
}

func result(turn int, extra map[string]any) map[string]any {
	r := map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn", "result": "ok",
		"modelUsage": map[string]any{
			"claude-test": map[string]any{"inputTokens": 15 * turn, "outputTokens": 4 * turn, "cacheReadInputTokens": 200 * turn, "cacheCreationInputTokens": 14 * turn, "costUSD": 0.01 * float64(turn)},
			"claude-sub":  map[string]any{"inputTokens": 1 * turn, "outputTokens": 0, "cacheReadInputTokens": 0, "cacheCreationInputTokens": 0, "costUSD": 0.001 * float64(turn)},
		},
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func fakeClaude(scenario string) {
	trace, _ := os.OpenFile(os.Getenv("CLAUDE_FAKE_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintf(trace, "env GH_TOKEN=%s\nargs %s\n", os.Getenv("GH_TOKEN"), strings.Join(os.Args[1:], " "))
	fmt.Fprintf(trace, "envs %s\n", tracedEnv())
	if scenario == "die" {
		fmt.Fprintln(os.Stderr, "Error: not logged in · run claude auth login")
	}
	in := bufio.NewScanner(os.Stdin)
	turn := 0
	for in.Scan() {
		line := in.Text()
		fmt.Fprintln(trace, line)
		var msg struct{ Type string }
		_ = json.Unmarshal([]byte(line), &msg)
		if msg.Type == "control_request" {
			if scenario != "stubborn" {
				emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "modelUsage": map[string]any{}})
			}
			continue
		}
		if msg.Type != "user" {
			continue
		}
		turn++
		emit(map[string]any{"type": "system", "subtype": "init", "model": "claude-test", "session_id": "s1"})
		switch scenario {
		case "ok", "garbage":
			if scenario == "garbage" {
				fmt.Println("not json at all")
				emit(map[string]any{"type": "some_future_event"})
			}
			tool := map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "go test ./...", "description": "Run tests"}}
			emit(assistantEvent("m1", 10, tool))
			emit(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "tu1", "is_error": true, "content": "Exit code 1\n--- FAIL: TestX (0.01s)\n    x_test.go:9: boom",
			}}}})
			emit(assistantEvent("m2", 5, map[string]any{"type": "text", "text": "DONE /tmp/x"}))
			emit(result(turn, nil))
		case "error":
			emit(result(turn, map[string]any{"is_error": true, "result": "API Error: 401 invalid x-api-key", "api_error_status": 401}))
		case "die":
			emit(assistantEvent("m1", 10, map[string]any{"type": "text", "text": "working"}))
			os.Exit(3)
		case "hang", "stubborn":
			// One response, as an event per content block that repeat its id and usage.
			emit(assistantEvent("m1", 10, map[string]any{"type": "text", "text": "thinking"}))
			emit(assistantEvent("m1", 10, map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "sleep 120"}}))
		default:
			for _, e := range hindsightScenarios[scenario] {
				emit(e)
			}
			emit(result(turn, nil))
		}
	}
	if scenario == "stubborn" {
		time.Sleep(time.Hour)
	}
}

type session struct {
	c     *Client
	log   *syncBuffer
	trace string
	usage string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func start(t *testing.T, scenario string, binaryArgs ...string) *session {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s := &session{log: &syncBuffer{}, trace: filepath.Join(dir, "trace"), usage: filepath.Join(dir, "s1"+UsageSuffix)}
	var transcript bytes.Buffer
	s.c, err = Start(context.Background(), Options{
		Binary: self, BinaryArgs: binaryArgs, Dir: dir, Args: []string{"--model", "opus"},
		Env:        []string{"CLAUDE_FAKE=" + scenario, "CLAUDE_FAKE_TRACE=" + s.trace, "GH_TOKEN=delamain-push-blocked://", "GORACE=atexit_sleep_ms=0"},
		Log:        s.log,
		Transcript: &transcript, UsagePath: s.usage, CloseTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.c.Close() })
	return s
}

func (s *session) traced(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.trace)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPromptShouldEndOnTheResultAndCountUsageCumulativelyWithSubagents(t *testing.T) {
	s := start(t, "ok")
	ctx := context.Background()
	first, err := s.c.Prompt(ctx, "plan it", time.Minute)
	if err != nil || first.StopReason != "end_turn" || first.Model != "claude-test" {
		t.Fatalf("turn 1: %+v %v", first, err)
	}
	second, err := s.c.Prompt(ctx, "now implement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// modelUsage is the session total over every model (subagents included): 16 per turn.
	if second.Usage.Input != 16 || second.Usage.CacheWrite != 14 {
		t.Fatalf("a turn's usage is its share of the cumulative total, got %+v", second.Usage)
	}
	if got := s.c.Spent(); got.Input != 32 || got.Output != 8 || got.CacheRead != 400 || got.CacheWrite != 28 || got.Cost.Total < 0.0219 || got.Cost.Total > 0.0221 {
		t.Fatalf("spent must be the session's cumulative total, got %+v", got)
	}
	u, err := ReadUsage(s.usage)
	if err != nil || u != s.c.Spent() {
		t.Fatalf("the usage file holds the cumulative total for the ledger: %+v %v", u, err)
	}
	if tr := s.traced(t); !strings.Contains(tr, `"type":"user"`) || !strings.Contains(tr, `"content":"now implement"`) || !strings.Contains(tr, "args -p --input-format stream-json --output-format stream-json --verbose --model opus") {
		t.Fatalf("prompts go as stream-json user messages after the stream flags:\n%s", tr)
	}
}

func TestLogShouldShowToolsTextAndWhyAToolFailed(t *testing.T) {
	s := start(t, "ok")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	for _, want := range []string{
		"▶ prompt (2 caracteres)",
		"🔧 Bash: Run tests",
		"✗ Bash falhou (exit 1): --- FAIL: TestX (0.01s) | x_test.go:9: boom",
		"💬 DONE /tmp/x",
		"■ fim do turno: end_turn · in 16 out 4 cacheW 14",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestPromptShouldReportAProviderErrorAsAnErrorTurn(t *testing.T) {
	s := start(t, "error")
	turn, err := s.c.Prompt(context.Background(), "go", time.Minute)
	if err != nil || turn.StopReason != "error" || !strings.Contains(turn.ErrorMessage, "api 401") || !strings.Contains(turn.ErrorMessage, "invalid x-api-key") {
		t.Fatalf("a failed result must not pass as done: %+v %v", turn, err)
	}
	if !strings.Contains(s.log.String(), "✗ success: api 401: API Error: 401 invalid x-api-key") {
		t.Fatalf("log: %s", s.log.String())
	}
}

func TestPromptShouldFailWithClaudesLastStderrLineWhenItExitsMidTurn(t *testing.T) {
	s := start(t, "die")
	_, err := s.c.Prompt(context.Background(), "go", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exit 3") || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("want the exit code and why, got %v", err)
	}
}

func TestPromptShouldSkipLinesThatAreNotEvents(t *testing.T) {
	s := start(t, "garbage")
	if turn, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil || turn.StopReason != "end_turn" {
		t.Fatalf("%+v %v", turn, err)
	}
}

func TestPromptShouldInterruptATurnThatOutlivesItsTimeoutAndKeepTheSession(t *testing.T) {
	s := start(t, "hang")
	_, err := s.c.Prompt(context.Background(), "go", 300*time.Millisecond)
	if !errors.Is(err, agent.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if tr := s.traced(t); !strings.Contains(tr, `"type":"control_request"`) || !strings.Contains(tr, `"subtype":"interrupt"`) {
		t.Fatalf("the turn must be interrupted:\n%s", tr)
	}
	if !strings.Contains(s.log.String(), "✗ abortando o turno") {
		t.Fatalf("log: %s", s.log.String())
	}
}

func TestPromptShouldKeepAndRecordTheUsageOfATurnThatTimedOut(t *testing.T) {
	s := start(t, "hang")
	if _, err := s.c.Prompt(context.Background(), "go", 300*time.Millisecond); !errors.Is(err, agent.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	// The interrupt's result carries no modelUsage: what the assistant reported must stay counted.
	if got := s.c.Spent(); got.Input != 10 || got.CacheWrite != 7 {
		t.Fatalf("the interrupted turn's usage was dropped: %+v", got)
	}
	u, err := ReadUsage(s.usage)
	if err != nil || u.Input != 10 || u.CacheWrite != 7 {
		t.Fatalf("the ledger must see a timed-out turn, often the costliest: %+v %v", u, err)
	}
}

func TestPromptShouldRecordTheUsageOfATurnWhoseProcessDied(t *testing.T) {
	s := start(t, "die")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err == nil {
		t.Fatal("want an error when claude exits mid-turn")
	}
	if u, err := ReadUsage(s.usage); err != nil || u.Input != 10 {
		t.Fatalf("usage reported before claude died must reach the ledger: %+v %v", u, err)
	}
}

func TestAbortShouldEndTheTurnAsAbortedAndCountLiveUsageOncePerMessage(t *testing.T) {
	s := start(t, "hang")
	type out struct {
		turn agent.Turn
		err  error
	}
	done := make(chan out, 1)
	go func() {
		turn, err := s.c.Prompt(context.Background(), "go", time.Minute)
		done <- out{turn, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for s.c.Spent().Input == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.c.Spent(); got.Input != 10 || got.CacheWrite != 7 {
		t.Fatalf("live usage counts a response once, not once per content block: %+v", got)
	}
	if err := s.c.Abort(); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != nil || o.turn.StopReason != "aborted" {
			t.Fatalf("%+v %v", o.turn, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("abort did not end the turn")
	}
}

func TestAbortShouldKillASessionThatIgnoresTheInterrupt(t *testing.T) {
	prev := abortGrace
	abortGrace = 300 * time.Millisecond
	t.Cleanup(func() { abortGrace = prev })
	s := start(t, "stubborn")
	done := make(chan agent.Turn, 1)
	go func() {
		turn, _ := s.c.Prompt(context.Background(), "go", time.Minute)
		done <- turn
	}()
	deadline := time.Now().Add(10 * time.Second)
	for s.c.Spent().Input == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = s.c.Abort()
	select {
	case turn := <-done:
		if turn.StopReason != "aborted" {
			t.Fatalf("a killed session still reports the turn as aborted: %+v", turn)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a session that ignores the interrupt must be killed")
	}
}

func TestStartShouldPutTheLauncherArgsBeforeStreamArgs(t *testing.T) {
	s := start(t, "ok", "launch", "claude")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	if tr := s.traced(t); !strings.Contains(tr, "args launch claude -p --input-format stream-json") {
		t.Fatalf("a launcher's subcommand must come before StreamArgs:\n%s", tr)
	}
}

func TestStartShouldPassTheEnvSoAgentsCannotPush(t *testing.T) {
	s := start(t, "ok")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	if tr := s.traced(t); !strings.Contains(tr, "env GH_TOKEN=delamain-push-blocked://") {
		t.Fatalf("the push-blocking env must reach claude:\n%s", tr)
	}
}

func TestUsageFilesShouldListEveryRolesSession(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"planner/a" + UsageSuffix, "reviewer/b" + UsageSuffix, "reviewer/b.stream.jsonl"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(`{"input":3}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := UsageFiles(dir)
	if err != nil || len(files) != 2 || !strings.HasSuffix(files[0], "planner/a"+UsageSuffix) {
		t.Fatalf("%v %v", files, err)
	}
	if u, err := ReadUsage(files[1]); err != nil || u.Input != 3 {
		t.Fatalf("%+v %v", u, err)
	}
	if files, err := UsageFiles(filepath.Join(dir, "absent")); err != nil || len(files) != 0 {
		t.Fatalf("a run without sessions has no usage: %v %v", files, err)
	}
}

// tracedKeys are the variables the fake agent reports, to check what it inherited.
var tracedKeys = []string{"GH_TOKEN", "GITHUB_TOKEN", "OAUTH_TOKEN", "X_SECRET", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

func tracedEnv() string {
	kv := make([]string, 0, len(tracedKeys))
	for _, k := range tracedKeys {
		kv = append(kv, k+"="+os.Getenv(k))
	}
	return strings.Join(kv, " ")
}

func setAgentSecrets(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GH_TOKEN": "s-gh", "GITHUB_TOKEN": "s-github", "OAUTH_TOKEN": "s-oauth", "X_SECRET": "s-x",
		"OPENAI_API_KEY": "k-openai", "CLAUDE_CODE_OAUTH_TOKEN": "c-oauth",
	} {
		t.Setenv(k, v)
	}
}

const wantAgentEnv = "envs GH_TOKEN=delamain-push-blocked:// GITHUB_TOKEN= OAUTH_TOKEN= X_SECRET= OPENAI_API_KEY=k-openai CLAUDE_CODE_OAUTH_TOKEN=c-oauth"

func TestStartShouldStripForgeTokensFromTheInheritedEnv(t *testing.T) {
	setAgentSecrets(t)
	s := start(t, "ok")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	if tr := s.traced(t); !strings.Contains(tr, wantAgentEnv+"\n") {
		t.Fatalf("claude inherited\n%s\nwant a line\n%s", tr, wantAgentEnv)
	}
}

// recallContext is what the Hindsight hook injects; its size is what the log reports.
const recallContext = "<hindsight_memory>o lint roda com make lint</hindsight_memory>"

// hookResponse is a system/hook_response as claude emits it with --include-hook-events: the
// hook's stdout is a JSON whose additionalContext is injected into the prompt.
func hookResponse(injected string, exitCode any) map[string]any {
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": injected}})
	return map[string]any{
		"type": "system", "subtype": "hook_response", "hook_id": "h1", "hook_name": "UserPromptSubmit", "hook_event": "UserPromptSubmit",
		"output": string(out) + "\n", "stdout": string(out) + "\n", "stderr": "", "exit_code": exitCode, "outcome": "success",
	}
}

// hindsightScenarios are the events each Hindsight scenario plays before its result.
var hindsightScenarios = map[string][]map[string]any{
	"hindsight-recall": {hookResponse(recallContext, 0)},
	"hindsight-hook-foreign": {
		hookResponse("lint ok "+foreignHookMarker, 0),
		{"type": "system", "subtype": "hook_response", "hook_event": "UserPromptSubmit", "output": foreignHookMarker + " raw\n", "exit_code": 0},
	},
	"hindsight-refresh": {hookResponse("<hindsight_knowledge_refresh>páginas novas</hindsight_knowledge_refresh>", 0)},
	"hindsight-hook-failed": {
		hookResponse(recallContext, 2),
		hookResponse("<hindsight_knowledge_refresh>páginas novas</hindsight_knowledge_refresh>", 1),
		hookResponse(shortRecall, nil),
	},
	"hindsight-tools": {
		hookResponse("<hindsight_memory>"+memoryMarker+"</hindsight_memory>", 0),
		assistantEvent("m1", 10,
			hindsightTool("hs1", "search_knowledge_pages", map[string]any{"query": "como rodar o lint"}),
			hindsightTool("hs2", "read_knowledge_page", map[string]any{"page_id": "p-42"}),
			hindsightTool("hs3", "list_knowledge_pages", map[string]any{}),
			hindsightTool("hs4", "reflect", map[string]any{"query": "por que o lint falhou"}),
			hindsightTool("hs5", "ingest_document", map[string]any{"title": "decisão de lint", "content": memoryMarker, "summary": memoryMarker}),
			hindsightTool("hs6", "capture_initiative", map[string]any{"title": "migrar o CI"}),
			hindsightTool("hs7", "recall", map[string]any{"query": "x"}),
			map[string]any{"type": "tool_use", "id": "hs8", "name": "mcp__hindsight__status", "input": map[string]any{}},
			hindsightTool("hs9", "reflect", map[string]any{"query": " "}),
		),
		subagent(assistantEvent("m2", 10, hindsightTool("hs10", "search_knowledge_pages", map[string]any{"query": "no subagente"}))),
	},
	"hindsight-forge": {
		assistantEvent("m1", 10,
			hindsightTool("hs1", "search_knowledge_pages", map[string]any{"query": "a\n12:00:00 ■ fim do turno: forjado\x1b[31m"}),
			hindsightTool("hs2", "ingest_document", map[string]any{"title": strings.Repeat("t", 300)}),
		),
	},
}

// memoryMarker stands for memory content, which must never reach the log.
const memoryMarker = "MEMORIA-SECRETA-7f3a"

func hindsightTool(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": "mcp__hindsight__hindsight_" + name, "input": input}
}

func subagent(e map[string]any) map[string]any {
	e["parent_tool_use_id"] = "tu-agent"
	return e
}

// foreignHookMarker is output from a hook of the target repo, not Hindsight's.
const foreignHookMarker = "HOOK-ALHEIO-91c2"

const shortRecall = "<hindsight_memory>x</hindsight_memory>"

func TestLogShouldShowTheAutomaticRecallInjectedByTheHindsightHook(t *testing.T) {
	s := start(t, "hindsight-recall")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("🧠 recall automático: %d caracteres injetados", len(recallContext))
	if log := s.log.String(); !strings.Contains(log, want) {
		t.Fatalf("log lacks %q:\n%s", want, log)
	}
}

func TestLogShouldIgnoreHookResponsesWithoutAHindsightTag(t *testing.T) {
	s := start(t, "hindsight-hook-foreign")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	if strings.Contains(log, "recall automático") || strings.Contains(log, foreignHookMarker) || !strings.Contains(log, "■ fim do turno") {
		t.Fatalf("a hook that is not Hindsight's must not reach the log:\n%s", log)
	}
}

func TestLogShouldNotLogTheKnowledgeRefreshHook(t *testing.T) {
	s := start(t, "hindsight-refresh")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	if log := s.log.String(); strings.Contains(log, "🧠") || !strings.Contains(log, "■ fim do turno") {
		t.Fatalf("a knowledge refresh is not a recall:\n%s", log)
	}
}

func TestLogShouldShowAFailedHindsightHook(t *testing.T) {
	s := start(t, "hindsight-hook-failed")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	for _, want := range []string{
		"✗ recall automático falhou (exit 2)",
		"✗ recall automático falhou (exit 1)",
		// No exit code is not a failure.
		fmt.Sprintf("🧠 recall automático: %d caracteres injetados", len(shortRecall)),
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if n := strings.Count(log, "🧠 recall automático"); n != 1 {
		t.Errorf("a failed hook injected nothing, want 1 recall line, got %d:\n%s", n, log)
	}
}

// logLines are the log's lines without their timestamp.
func logLines(log string) []string {
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(log, "\n"), "\n") {
		_, msg, _ := strings.Cut(l, " ")
		lines = append(lines, msg)
	}
	return lines
}

func TestLogShouldShowHindsightToolsWithTheirQueryOrTitle(t *testing.T) {
	s := start(t, "hindsight-tools")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	lines := logLines(log)
	for _, want := range []string{
		`🧠 busca nas páginas: "como rodar o lint"`,
		"🧠 lendo página: p-42",
		"🧠 listando páginas",
		`🧠 reflect: "por que o lint falhou"`,
		`🧠 retain: "decisão de lint"`,
		`🧠 iniciativa registrada: "migrar o CI"`,
		"🧠 recall",
		"🧠 status",
		// An empty query leaves no empty quotes behind.
		"🧠 reflect",
		`⑂ 🧠 busca nas páginas: "no subagente"`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("log lacks the line %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "🔧") {
		t.Errorf("a Hindsight tool gets only its 🧠 line:\n%s", log)
	}
}

func TestLogShouldKeepHindsightToolDetailsOnASingleLine(t *testing.T) {
	s := start(t, "hindsight-forge")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	lines := logLines(log)
	for _, want := range []string{
		`🧠 busca nas páginas: "a 12:00:00 ■ fim do turno: forjado [31m"`,
		`🧠 retain: "` + strings.Repeat("t", 160) + `…"`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("log lacks the line %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "\x1b") || slices.Contains(lines, "■ fim do turno: forjado\x1b[31m\"") {
		t.Errorf("a tool input must not forge log lines nor reach the terminal raw:\n%q", log)
	}
}

func TestLogShouldNeverLogHindsightMemoryContent(t *testing.T) {
	s := start(t, "hindsight-tools")
	if _, err := s.c.Prompt(context.Background(), "go", time.Minute); err != nil {
		t.Fatal(err)
	}
	log := s.log.String()
	if strings.Contains(log, memoryMarker) || !strings.Contains(log, "🧠 recall automático") || !strings.Contains(log, "🧠 retain") {
		t.Fatalf("memory content leaked, or the Hindsight lines are missing:\n%s", log)
	}
}
