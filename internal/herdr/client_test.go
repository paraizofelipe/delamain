package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeRunner returns canned output per command prefix and records calls.
type fakeRunner struct {
	calls     [][]string
	responses map[string]fakeResp
}

type fakeResp struct {
	stdout, stderr string
	err            error
}

func (f *fakeRunner) Run(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	key := strings.Join(args, " ")
	for prefix, r := range f.responses {
		if strings.HasPrefix(key, prefix) {
			return []byte(r.stdout), []byte(r.stderr), r.err
		}
	}
	return nil, nil, errors.New("unexpected call: " + key)
}

// Fixtures captured from herdr 0.9.3 during the M0 spike.
const (
	worktreeCreateJSON = `{"id":"cli:worktree:create","result":{"root_pane":{"pane_id":"w2:p1"},"tab":{"tab_id":"w2:t1"},"workspace":{"workspace_id":"w2"},"worktree":{"branch":"fix/1-add","is_linked_worktree":true,"path":"/tmp/wt-fix-1-add"}}}`
	agentPromptJSON    = `{"id":"cli:agent:prompt","result":{"agent":{"agent":"omp","agent_session":{"agent":"omp","kind":"path","source":"herdr:omp","value":"/tmp/sessions/implementer/s.jsonl"},"agent_status":"done","cwd":"/tmp/wt","interactive_ready":true,"name":"implementer","pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"}}}`
	agentNotFoundJSON  = `{"error":{"code":"agent_not_found","message":"agent target nonexistent not found"},"id":"cli:agent:get"}`
	integrationStatus  = "pi: not installed (/home/u/.pi/agent/extensions/herdr-agent-state.ts)\nomp: current (v10) (/home/u/.omp/agent/extensions/herdr-omp-agent-state.ts)\n"
	tabCreateJSON      = `{"id":"cli:tab:create","result":{"root_pane":{"agent_status":"unknown","pane_id":"w2D:p2","tab_id":"w2D:t2","workspace_id":"w2D"},"tab":{"agent_status":"unknown","focused":false,"label":"planner","number":2,"tab_id":"w2D:t2"},"type":"tab_created"}}` // herdr 0.9.3, tab spike
	sessionListJSON    = `{"sessions":[{"default":true,"name":"default","running":true,"socket_path":"/d.sock"},{"default":false,"name":"delamain-spike","running":true,"socket_path":"/s.sock"},{"default":false,"name":"old","running":false,"socket_path":"/o.sock"}]}`
)

func TestWorktreeCreateShouldParseIDsAndNeverStealFocus(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"worktree create": {stdout: worktreeCreateJSON}}}
	wt, err := New(f).WorktreeCreate(context.Background(), WorktreeOptions{Cwd: "/repo", Branch: "fix/1-add", Base: "main", Path: "/tmp/wt-fix-1-add", Label: "#1"})
	if err != nil {
		t.Fatal(err)
	}
	want := Worktree{WorkspaceID: "w2", TabID: "w2:t1", RootPaneID: "w2:p1", Path: "/tmp/wt-fix-1-add", Branch: "fix/1-add"}
	if wt != want {
		t.Fatalf("got %+v want %+v", wt, want)
	}
	if !slices.Contains(f.calls[0], "--no-focus") {
		t.Fatalf("background work must not steal the user's focus: %v", f.calls[0])
	}
}

func TestWorktreeCreateShouldFailWhenResponseHasNoPane(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"worktree create": {stdout: `{"result":{}}`}}}
	if _, err := New(f).WorktreeCreate(context.Background(), WorktreeOptions{Cwd: "/r", Branch: "b"}); err == nil {
		t.Fatal("a response without a pane id cannot be used and must fail")
	}
}

func TestTabCreateShouldParseIDsAndNeverStealFocus(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"tab create": {stdout: tabCreateJSON}}}
	tab, err := New(f).TabCreate(context.Background(), "w2D", "/tmp/wt", "planner")
	if err != nil {
		t.Fatal(err)
	}
	if tab != (Tab{ID: "w2D:t2", RootPaneID: "w2D:p2"}) {
		t.Fatalf("got %+v", tab)
	}
	want := []string{"tab", "create", "--workspace", "w2D", "--cwd", "/tmp/wt", "--label", "planner"}
	if !slices.Equal(f.calls[0], want) {
		t.Fatalf("args = %v, want %v (no --focus: background work must not steal the user's focus)", f.calls[0], want)
	}
}

func TestTabCreateShouldFailWhenResponseHasNoPane(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"tab create": {stdout: `{"result":{"tab":{"tab_id":"w2D:t2"}}}`}}}
	if _, err := New(f).TabCreate(context.Background(), "w2D", "/tmp/wt", "planner"); err == nil {
		t.Fatal("a tab without a pane cannot host an agent and must fail")
	}
}

func TestTabRenameShouldSendTheLabelAsOneArgument(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"tab rename": {stdout: `{"result":{}}`}}}
	if err := New(f).TabRename(context.Background(), "w2D:t2", "planner ✓"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"tab", "rename", "w2D:t2", "planner ✓"}; !slices.Equal(f.calls[0], want) {
		t.Fatalf("args = %v, want %v", f.calls[0], want)
	}
}

func TestAgentPromptShouldExposeSessionPathForTokenAccounting(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent prompt": {stdout: agentPromptJSON}}}
	a, err := New(f).AgentPrompt(context.Background(), "implementer", "do it", 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusDone || a.Session.Value != "/tmp/sessions/implementer/s.jsonl" || a.Kind != "omp" {
		t.Fatalf("unexpected agent: %+v", a)
	}
	want := []string{"agent", "prompt", "implementer", "do it", "--wait", "--timeout", "90000"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("args %v want %v", f.calls[0], want)
	}
}

func TestAgentStartShouldPassAgentArgsAfterDoubleDash(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent start": {stdout: `{"result":{"agent":{"name":"planner","agent_status":"idle"}}}`}}}
	_, err := New(f).AgentStart(context.Background(), "planner", "omp", "w2:p1", time.Minute, "--model", "m", "--approval-mode", "yolo")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	if !strings.HasSuffix(got, "--timeout 60000 -- --model m --approval-mode yolo") {
		t.Fatalf("native agent args must follow --: %s", got)
	}
}

func TestRunShouldDecodeStructuredErrorFromStderr(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent get": {stderr: agentNotFoundJSON, err: errors.New("exit status 1")}}}
	_, err := New(f).AgentGet(context.Background(), "nonexistent")
	if !IsCode(err, CodeAgentNotFound) {
		t.Fatalf("want agent_not_found, got %v", err)
	}
}

func TestRunShouldDecodeStructuredErrorPrintedOnStdout(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent get": {stdout: agentNotFoundJSON, err: errors.New("exit status 1")}}}
	_, err := New(f).AgentGet(context.Background(), "x")
	if !IsCode(err, CodeAgentNotFound) {
		t.Fatalf("want agent_not_found from stdout fallback, got %v", err)
	}
}

func TestRunShouldWrapUnstructuredFailureWithStderrText(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent get": {stderr: "boom", err: errors.New("exit status 9")}}}
	_, err := New(f).AgentGet(context.Background(), "x")
	var he *Error
	if !errors.As(err, &he) || he.Message != "boom" {
		t.Fatalf("want wrapped error carrying stderr, got %v", err)
	}
}

func TestRunShouldReportInvalidJSONResponse(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent get": {stdout: "not json"}}}
	if _, err := New(f).AgentGet(context.Background(), "x"); err == nil {
		t.Fatal("garbage output must not be treated as success")
	}
}

func TestIntegrationStatusShouldReturnStateWithoutVersionAndPath(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"integration status": {stdout: integrationStatus}}}
	c := New(f)
	if s, err := c.IntegrationStatus(context.Background(), "omp"); err != nil || s != "current" {
		t.Fatalf("omp: %q %v", s, err)
	}
	if s, _ := c.IntegrationStatus(context.Background(), "pi"); s != "not installed" {
		t.Fatalf("pi: %q", s)
	}
	if _, err := c.IntegrationStatus(context.Background(), "nope"); err == nil {
		t.Fatal("unlisted integration must be an error")
	}
}

func TestSocketPathShouldResolveDefaultNamedAndStoppedSessions(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"session list": {stdout: sessionListJSON}}}
	c := New(f)
	if p, err := c.SocketPath(context.Background(), ""); err != nil || p != "/d.sock" {
		t.Fatalf("default: %q %v", p, err)
	}
	if p, err := c.SocketPath(context.Background(), "delamain-spike"); err != nil || p != "/s.sock" {
		t.Fatalf("named: %q %v", p, err)
	}
	if _, err := c.SocketPath(context.Background(), "old"); err == nil {
		t.Fatal("a stopped session has no live socket")
	}
	if _, err := c.SocketPath(context.Background(), "ghost"); err == nil {
		t.Fatal("unknown session must fail")
	}
}

// fakeBinary writes an executable shell script that stands in for herdr.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "herdr")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecRunnerShouldPinNamedSessionBeforeSubcommand(t *testing.T) {
	bin := fakeBinary(t, `printf '{"result":{"args":"%s"}}' "$*"`)
	out, _, err := ExecRunner{Binary: bin, Session: "delamain-test"}.Run(context.Background(), "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"args":"--session delamain-test agent list"`) {
		t.Fatalf("session flag must precede the subcommand: %s", out)
	}
}

func TestClientShouldMapExitCodeTwoToUsageError(t *testing.T) {
	bin := fakeBinary(t, "echo 'unknown subcommand' >&2; exit 2")
	_, err := New(ExecRunner{Binary: bin}).AgentList(context.Background())
	var he *Error
	if !errors.As(err, &he) || he.Code != codeUsage || he.ExitCode != 2 {
		t.Fatalf("exit 2 is a CLI syntax error, got %+v", err)
	}
}

func TestErrorShouldNotEmbedPromptTextInMessage(t *testing.T) {
	prompt := strings.Repeat("instrução longa\n", 50)
	f := &fakeRunner{responses: map[string]fakeResp{"agent prompt": {stderr: `{"error":{"code":"server_unavailable","message":"server is shutting down"}}`, err: errors.New("exit 1")}}}
	_, err := New(f).AgentPrompt(context.Background(), "implementer-r1", prompt, time.Second)
	msg := err.Error()
	if strings.Contains(msg, "instrução longa") || len(msg) > 200 {
		t.Fatalf("error must stay short and payload-free: %q", msg)
	}
	if !strings.Contains(msg, "agent prompt implementer-r1") || !strings.Contains(msg, "server is shutting down") {
		t.Fatalf("error must keep command, target and cause: %q", msg)
	}
}

func TestMutatingCommandsShouldAcceptEmptySuccessOutput(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"pane run": {stdout: ""}, "agent send-keys": {stdout: "\n"}, "agent get": {stdout: ""}}}
	c := New(f)
	if err := c.PaneRun(context.Background(), "w1:p1", "go test ./..."); err != nil {
		t.Fatalf("herdr prints nothing on a successful pane run: %v", err)
	}
	if err := c.AgentSendKeys(context.Background(), "a", "ctrl+d"); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	if _, err := c.AgentGet(context.Background(), "a"); err == nil {
		t.Fatal("a query that must return data cannot succeed on empty output")
	}
}

func TestWorkspaceReportMetadataShouldSendSortedTokensWithSeq(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"workspace report-metadata": {}}} // herdr prints nothing on success
	err := New(f).WorkspaceReportMetadata(context.Background(), "w2", "delamain", 42, map[string]string{"tokens": "167K/2M", "iter": "1/3"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(f.calls[0], " ")
	if got != "workspace report-metadata w2 --source delamain --seq 42 --token iter=1/3 --token tokens=167K/2M" {
		t.Fatalf("args = %s", got)
	}
}
