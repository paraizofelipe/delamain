package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake omp: with OMP_FAKE set it speaks the RPC protocol and
// plays the scenario named by the variable. OMP_FAKE_TRACE receives every stdin line.
func TestMain(m *testing.M) {
	if s := os.Getenv("OMP_FAKE"); s != "" {
		fakeOmp(s)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func assistant(stop, errMsg string, input int64) map[string]any {
	return map[string]any{
		"role": "assistant", "model": "gpt-test", "stopReason": stop, "errorMessage": errMsg,
		"content": []any{map[string]any{"type": "text", "text": "DONE /tmp/x"}},
		"usage":   map[string]any{"input": input, "output": 5, "cacheRead": 100, "cacheWrite": 7, "cost": map[string]any{"total": 0.5}},
	}
}

func fakeOmp(scenario string) {
	trace, _ := os.OpenFile(os.Getenv("OMP_FAKE_TRACE"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintf(trace, "env GH_TOKEN=%s\n", os.Getenv("GH_TOKEN"))
	if scenario == "noready" {
		time.Sleep(time.Hour)
	}
	if scenario == "stubborn" {
		// Ignores stdin EOF and leaves a child behind: only a group kill stops both.
		_ = exec.Command("sleep", "3600").Start()
		signal.Ignore(syscall.SIGTERM)
	}
	if scenario == "leaky" {
		// Exits cleanly on EOF but leaves a tool process behind in its group.
		_ = exec.Command("sleep", "3600").Start()
	}
	emit(map[string]any{"type": "ready", "protocolVersion": 1})
	emit(map[string]any{"type": "extension_ui_request", "id": "w1", "method": "setWidget", "widgetKey": "x"})
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		line := in.Text()
		fmt.Fprintln(trace, line)
		var cmd struct{ ID, Type string }
		_ = json.Unmarshal([]byte(line), &cmd)
		switch cmd.Type {
		case "abort":
			emit(map[string]any{"id": cmd.ID, "type": "response", "command": "abort", "success": true})
			emit(map[string]any{"type": "message_end", "message": assistant("aborted", "Interrupted by user", 0)})
			emit(map[string]any{"type": "agent_end", "messages": []any{}})
		case "prompt":
			playPrompt(scenario, cmd.ID, in, trace)
		}
	}
	if scenario == "stubborn" {
		time.Sleep(time.Hour)
	}
}

func playPrompt(scenario, id string, in *bufio.Scanner, trace io.Writer) {
	ack := map[string]any{"id": id, "type": "response", "command": "prompt", "success": true}
	switch scenario {
	case "ok", "stubborn":
		emit(map[string]any{"type": "agent_start"})
		emit(ack)
		emit(map[string]any{"type": "tool_execution_start", "toolName": "bash", "intent": "rodar testes", "args": map[string]any{"command": "go test"}})
		emit(map[string]any{"type": "message_end", "message": map[string]any{"role": "user", "content": "x"}})
		emit(map[string]any{"type": "message_end", "message": assistant("toolUse", "", 900)})
		emit(map[string]any{"type": "turn_end", "message": assistant("toolUse", "", 900)}) // repeats the message: not new usage
		emit(map[string]any{"type": "agent_end", "isTerminal": false})                     // async continuation: not the end
		emit(map[string]any{"type": "message_end", "message": assistant("stop", "", 1200)})
		emit(map[string]any{"type": "agent_end", "messages": []any{}})
	case "error401":
		emit(ack)
		emit(map[string]any{"type": "message_end", "message": assistant("error", "401 Invalid API Key", 0)})
		emit(map[string]any{"type": "agent_end"})
	case "nocreds":
		emit(ack)
		emit(map[string]any{"id": id, "type": "response", "command": "prompt", "success": false, "error": "No API key found for groq."})
	case "local":
		emit(map[string]any{"id": id, "type": "response", "command": "prompt", "success": true, "data": map[string]any{"agentInvoked": false}})
	case "garbage":
		fmt.Println("this is not json")
		emit(map[string]any{"type": "brand_new_event", "x": 1})
		emit(map[string]any{"type": "notice", "level": "warning", "message": "context almost full"})
		emit(ack)
		emit(map[string]any{"type": "extension_ui_request", "id": "ui-7", "method": "confirm", "title": "ok?", "message": "?"})
		// Like omp, the dialog blocks the turn until the client answers it.
		if in.Scan() {
			fmt.Fprintln(trace, in.Text())
		}
		emit(map[string]any{"type": "message_end", "message": assistant("stop", "", 10)})
		emit(map[string]any{"type": "agent_end"})
	case "hang":
		emit(ack) // never ends on its own: only abort finishes the turn
		emit(map[string]any{"type": "message_end", "message": assistant("toolUse", "", 4000)})
	case "die":
		emit(ack)
		os.Exit(3)
	}
}

type harness struct {
	trace string
	log   *syncBuffer
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

func start(t *testing.T, scenario string, mod ...func(*Options)) (*Client, *harness) {
	t.Helper()
	h := &harness{trace: filepath.Join(t.TempDir(), "trace"), log: &syncBuffer{}}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Binary: self, Dir: t.TempDir(), Args: []string{"--mode", "rpc"},
		Env: []string{"OMP_FAKE=" + scenario, "OMP_FAKE_TRACE=" + h.trace, "GORACE=atexit_sleep_ms=0"},
		Log: h.log, ReadyTimeout: 5 * time.Second, CloseTimeout: 2 * time.Second,
	}
	for _, m := range mod {
		m(&o)
	}
	c, err := Start(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, h
}

func (h *harness) lines(t *testing.T) []string {
	t.Helper()
	b, _ := os.ReadFile(h.trace)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestPromptShouldReturnTheFinalAssistantMessageOfTheTurn(t *testing.T) {
	c, h := start(t, "ok")
	turn, err := c.Prompt(context.Background(), "implemente", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if turn.StopReason != "stop" || turn.Failed() || turn.Model != "gpt-test" || turn.Usage.Input != 1200 {
		t.Fatalf("a non-terminal agent_end must not end the turn early: %+v", turn)
	}
	log := h.log.String()
	for _, want := range []string{"bash", "rodar testes", "DONE /tmp/x", "fim do turno"} {
		if !strings.Contains(log, want) {
			t.Errorf("the human log must show %q:\n%s", want, log)
		}
	}
}

func TestPromptShouldSurfaceProviderErrorsFromTheTurn(t *testing.T) {
	c, _ := start(t, "error401")
	turn, err := c.Prompt(context.Background(), "x", 5*time.Second)
	if err != nil || !turn.Failed() || !strings.Contains(turn.ErrorMessage, "401") {
		t.Fatalf("a provider error is a failed turn, not a success: %+v err=%v", turn, err)
	}
}

func TestPromptShouldEndWhenTheScheduledPromptFailsWithoutAgentEnd(t *testing.T) {
	c, _ := start(t, "nocreds")
	began := time.Now()
	_, err := c.Prompt(context.Background(), "x", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "No API key") {
		t.Fatalf("missing credentials must fail the prompt, got %v", err)
	}
	if time.Since(began) > 2*time.Second {
		t.Fatal("the same-id failure response ends the turn; it must not wait for the timeout")
	}
}

func TestPromptShouldEndWhenTheAgentIsNotInvoked(t *testing.T) {
	c, _ := start(t, "local")
	if _, err := c.Prompt(context.Background(), "/compact", 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestPromptShouldIgnoreUnknownFramesAndCancelInteractiveUIRequests(t *testing.T) {
	c, h := start(t, "garbage")
	if turn, err := c.Prompt(context.Background(), "x", 5*time.Second); err != nil || turn.StopReason != "stop" {
		t.Fatalf("got %+v %v", turn, err)
	}
	if !strings.Contains(h.log.String(), "context almost full") {
		t.Errorf("notices must reach the human log:\n%s", h.log.String())
	}
	var reply string
	for _, l := range h.lines(t) {
		if strings.Contains(l, "extension_ui_response") {
			reply = l
		}
	}
	if !strings.Contains(reply, `"id":"ui-7"`) || !strings.Contains(reply, `"cancelled":true`) {
		t.Fatalf("an unanswered dialog deadlocks omp; it must be cancelled, trace=%v", h.lines(t))
	}
	if strings.Contains(strings.Join(h.lines(t), "\n"), `"id":"w1"`) {
		t.Fatal("fire-and-forget UI requests take no reply")
	}
}

func TestSpentShouldSumEveryAssistantMessageAcrossTurns(t *testing.T) {
	c, _ := start(t, "ok")
	for range 2 {
		if _, err := c.Prompt(context.Background(), "x", 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// Per turn: 900 + 1200 input. turn_end repeats a message and user messages carry no usage.
	got := c.Spent()
	if got.Input != 4200 || got.Output != 20 || got.CacheWrite != 28 || got.Cost.Total != 2 {
		t.Fatalf("spent = %+v", got)
	}
}

func TestSpentShouldReportUsageWhileATurnIsInFlight(t *testing.T) {
	c, _ := start(t, "hang")
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		_, _ = c.Prompt(context.Background(), "x", 10*time.Second)
	}()
	// The budget watcher reads this mid-turn, before omp writes the session file.
	deadline := time.Now().Add(5 * time.Second)
	for c.Spent().Input != 4000 {
		if time.Now().After(deadline) {
			t.Fatalf("usage of the turn in flight never showed up: %+v", c.Spent())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = c.Abort()
	<-ended
}

func TestPromptShouldAbortTheTurnWhenItTimesOut(t *testing.T) {
	c, h := start(t, "hang")
	_, err := c.Prompt(context.Background(), "x", 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(strings.Join(h.lines(t), "\n"), `"type":"abort"`) {
		t.Fatal("a timed-out turn must be aborted so it stops spending")
	}
}

func TestAbortShouldEndTheRunningTurnAsAborted(t *testing.T) {
	c, _ := start(t, "hang")
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = c.Abort()
	}()
	turn, err := c.Prompt(context.Background(), "x", 10*time.Second)
	if err != nil || turn.StopReason != "aborted" {
		t.Fatalf("got %+v %v", turn, err)
	}
}

func TestPromptShouldFailWhenOmpExitsMidTurn(t *testing.T) {
	c, _ := start(t, "die")
	if _, err := c.Prompt(context.Background(), "x", 5*time.Second); err == nil || !strings.Contains(err.Error(), "omp") {
		t.Fatalf("a dead agent must fail the turn instead of hanging, got %v", err)
	}
}

func TestStartShouldFailWhenOmpNeverBecomesReady(t *testing.T) {
	_, err := func() (*Client, error) {
		self, _ := os.Executable()
		return Start(context.Background(), Options{
			Binary: self, Dir: t.TempDir(), Env: []string{"OMP_FAKE=noready"},
			Log: &syncBuffer{}, ReadyTimeout: 300 * time.Millisecond, CloseTimeout: time.Second,
		})
	}()
	if err == nil {
		t.Fatal("an agent that never signals ready must not be used")
	}
}

func TestStartShouldLetOptionsEnvOverrideTheInheritedOne(t *testing.T) {
	t.Setenv("GH_TOKEN", "real-token")
	_, h := start(t, "ok", func(o *Options) { o.Env = append(o.Env, "GH_TOKEN=blocked://") })
	if got := h.lines(t)[0]; got != "env GH_TOKEN=blocked://" {
		t.Fatalf("the push-blocking env must win over the user's token, got %q", got)
	}
}

func TestCloseShouldKillTheWholeProcessGroupWhenOmpIgnoresEOF(t *testing.T) {
	c, _ := start(t, "stubborn")
	pgid := c.PID()
	began := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatal("close must not wait forever for a stubborn agent")
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the agent's children must die with it, kill -0 = %v", err)
	}
}

func TestCloseShouldKillToolsLeftBehindByACleanExit(t *testing.T) {
	c, _ := start(t, "leaky")
	pgid := c.PID()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pgid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("a tool omp left running must not outlive the session, kill -0 = %v", err)
	}
}

func TestToolFailureShouldShowTheExitCodeAndTheLineThatExplainsIt(t *testing.T) {
	cases := map[string]struct{ out, want string }{
		"make echoes its recipe before the failing test": {
			out:  "timeout 180s go test -race ./...\nok  \tx/a\t1.0s\n--- FAIL: TestPush (0.06s)\n    gitx_test.go:140: git push: exit status 128\nFAIL\n\n\nWall time: 14.42 seconds\nCommand exited with code 2",
			want: " (exit 2): --- FAIL: TestPush (0.06s) | gitx_test.go:140: git push: exit status 128",
		},
		"no error-looking line: the first one": {
			out:  "something odd\nmore\nCommand exited with code 1",
			want: " (exit 1): something odd | more",
		},
		"a non-bash tool without exit code": {
			out:  "Path '/x/run_test.go' not found",
			want: ": Path '/x/run_test.go' not found",
		},
		"only omp's trailer": {
			out:  "\n[raw output: artifact://2]\nCommand exited with code 3\n",
			want: " (exit 3)",
		},
		"empty output": {},
	}
	for name, tc := range cases {
		if got := toolFailure(tc.out); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
	if got := toolFailure("error: " + strings.Repeat("x", 1000)); len(got) > 310 {
		t.Errorf("a huge line is truncated, got %d bytes", len(got))
	}
}

func TestLogFrameShouldExplainAFailedToolFromItsResult(t *testing.T) {
	var buf bytes.Buffer
	c := &Client{log: &buf}
	var f frame
	if err := json.Unmarshal([]byte(`{"type":"tool_execution_end","toolName":"bash","isError":true,
		"result":{"content":[{"type":"text","text":"--- FAIL: TestX\nCommand exited with code 1"}]}}`), &f); err != nil {
		t.Fatal(err)
	}
	c.logFrame(f)
	if got := buf.String(); !strings.HasSuffix(got, "✗ bash falhou (exit 1): --- FAIL: TestX\n") {
		t.Fatalf("got %q", got)
	}
	buf.Reset()
	f.IsError = false
	if c.logFrame(f); buf.Len() != 0 {
		t.Fatalf("a successful tool logs nothing on end: %q", buf.String())
	}
}
