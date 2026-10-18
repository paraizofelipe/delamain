package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	// Shape documented for `workspace create` (herdr cli-reference 0.9.x): same ids as worktree create.
	workspaceCreateJSON = `{"id":"cli:workspace:create","result":{"root_pane":{"pane_id":"w2:p1"},"tab":{"tab_id":"w2:t1"},"workspace":{"workspace_id":"w2"}}}`
	agentListJSON       = `{"id":"cli:agent:list","result":{"agents":[{"agent":"omp","agent_status":"done","cwd":"/tmp/wt","name":"implementer","pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2"}]}}`
	serverDownJSON      = `{"error":{"code":"server_unavailable","message":"server is shutting down"},"id":"cli:agent:list"}`
	tabCreateJSON       = `{"id":"cli:tab:create","result":{"root_pane":{"agent_status":"unknown","pane_id":"w2D:p2","tab_id":"w2D:t2","workspace_id":"w2D"},"tab":{"agent_status":"unknown","focused":false,"label":"planner","number":2,"tab_id":"w2D:t2"},"type":"tab_created"}}` // herdr 0.9.3, tab spike
	sessionListJSON     = `{"sessions":[{"default":true,"name":"default","running":true,"socket_path":"/d.sock"},{"default":false,"name":"delamain-spike","running":true,"socket_path":"/s.sock"},{"default":false,"name":"old","running":false,"socket_path":"/o.sock"}]}`
)

func TestWorkspaceCreateShouldParseIDsAndNeverStealFocus(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"workspace create": {stdout: workspaceCreateJSON}}}
	ws, err := New(f).WorkspaceCreate(context.Background(), "/wt/fix-1-add", "#1 add", []string{"ZDOTDIR=/q"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (Workspace{ID: "w2", TabID: "w2:t1", RootPaneID: "w2:p1"}); ws != want {
		t.Fatalf("got %+v want %+v", ws, want)
	}
	want := []string{"workspace", "create", "--cwd", "/wt/fix-1-add", "--no-focus", "--label", "#1 add", "--env", "ZDOTDIR=/q"}
	if !slices.Equal(f.calls[0], want) {
		t.Fatalf("background work must open at the worktree without stealing focus: %v", f.calls[0])
	}
}

func TestWorkspaceCreateShouldFailWhenResponseHasNoPane(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"workspace create": {stdout: `{"result":{"workspace":{"workspace_id":"w2"}}}`}}}
	if _, err := New(f).WorkspaceCreate(context.Background(), "/r", "", nil); err == nil {
		t.Fatal("a workspace without a pane cannot show anything")
	}
}

func TestTabCreateShouldParseIDsAndNeverStealFocus(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"tab create": {stdout: tabCreateJSON}}}
	tab, err := New(f).TabCreate(context.Background(), "w2D", "/tmp/wt", "planner", []string{"ZDOTDIR=/q"})
	if err != nil {
		t.Fatal(err)
	}
	if tab != (Tab{ID: "w2D:t2", RootPaneID: "w2D:p2"}) {
		t.Fatalf("got %+v", tab)
	}
	want := []string{"tab", "create", "--workspace", "w2D", "--cwd", "/tmp/wt", "--label", "planner", "--env", "ZDOTDIR=/q"}
	if !slices.Equal(f.calls[0], want) {
		t.Fatalf("args = %v, want %v (no --focus: background work must not steal the user's focus)", f.calls[0], want)
	}
}

func TestTabCreateShouldFailWhenResponseHasNoPane(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"tab create": {stdout: `{"result":{"tab":{"tab_id":"w2D:t2"}}}`}}}
	if _, err := New(f).TabCreate(context.Background(), "w2D", "/tmp/wt", "planner", nil); err == nil {
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

func TestAgentListShouldParseTheWorkspaceOfEachAgent(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent list": {stdout: agentListJSON}}}
	as, err := New(f).AgentList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The restored-workspace guard matches agents by workspace.
	if len(as) != 1 || as[0].Name != "implementer" || as[0].WorkspaceID != "w2" || as[0].Status != "done" {
		t.Fatalf("unexpected agents: %+v", as)
	}
}

func TestRunShouldDecodeStructuredErrorFromStderr(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent list": {stderr: serverDownJSON, err: errors.New("exit status 1")}}}
	_, err := New(f).AgentList(context.Background())
	if !IsCode(err, "server_unavailable") {
		t.Fatalf("want server_unavailable, got %v", err)
	}
}

func TestRunShouldDecodeStructuredErrorPrintedOnStdout(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent list": {stdout: serverDownJSON, err: errors.New("exit status 1")}}}
	_, err := New(f).AgentList(context.Background())
	if !IsCode(err, "server_unavailable") {
		t.Fatalf("want server_unavailable from stdout fallback, got %v", err)
	}
}

func TestRunShouldWrapUnstructuredFailureWithStderrText(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent list": {stderr: "boom", err: errors.New("exit status 9")}}}
	_, err := New(f).AgentList(context.Background())
	var he *Error
	if !errors.As(err, &he) || he.Message != "boom" {
		t.Fatalf("want wrapped error carrying stderr, got %v", err)
	}
}

func TestRunShouldReportInvalidJSONResponse(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"agent list": {stdout: "not json"}}}
	if _, err := New(f).AgentList(context.Background()); err == nil {
		t.Fatal("garbage output must not be treated as success")
	}
}

func TestSessionsShouldReportWhichSessionsAreRunning(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"session list": {stdout: sessionListJSON}}}
	ss, err := New(f).Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// doctor requires a running server: a stopped session must not look alive.
	if len(ss) != 3 || !ss[0].Default || !ss[0].Running || ss[2].Name != "old" || ss[2].Running {
		t.Fatalf("unexpected sessions: %+v", ss)
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

func TestErrorShouldNotEmbedCommandPayloadInMessage(t *testing.T) {
	payload := strings.Repeat("comando longo\n", 50)
	f := &fakeRunner{responses: map[string]fakeResp{"pane run": {stderr: serverDownJSON, err: errors.New("exit 1")}}}
	err := New(f).PaneRun(context.Background(), "w2:p1", payload)
	msg := err.Error()
	if strings.Contains(msg, "comando longo") || len(msg) > 200 {
		t.Fatalf("error must stay short and payload-free: %q", msg)
	}
	if !strings.Contains(msg, "pane run w2:p1") || !strings.Contains(msg, "server is shutting down") {
		t.Fatalf("error must keep command, target and cause: %q", msg)
	}
}

func TestMutatingCommandsShouldAcceptEmptySuccessOutput(t *testing.T) {
	f := &fakeRunner{responses: map[string]fakeResp{"pane run": {stdout: ""}, "tab rename": {stdout: "\n"}, "agent list": {stdout: ""}}}
	c := New(f)
	if err := c.PaneRun(context.Background(), "w1:p1", "go test ./..."); err != nil {
		t.Fatalf("herdr prints nothing on a successful pane run: %v", err)
	}
	if err := c.TabRename(context.Background(), "w1:t2", "planner ✓"); err != nil {
		t.Fatalf("tab rename: %v", err)
	}
	if _, err := c.AgentList(context.Background()); err == nil {
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
