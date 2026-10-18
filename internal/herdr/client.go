// Package herdr drives the herdr CLI (JSON responses) and its event socket.
//
// Contract notes verified in the M0 spike (herdr 0.9.3):
//   - errors are JSON {"error":{"code","message"}} with exit status 1; syntax errors exit 2.
//   - `agent read` and `pane read` print raw text, not JSON.
//   - agent state alone never proves completion: callers must verify artifacts and the omp session.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Runner executes the herdr binary. The exec implementation is swapped for a fake in tests.
type Runner interface {
	Run(ctx context.Context, args ...string) (stdout, stderr []byte, err error)
}

// ExecRunner runs a real herdr binary, optionally pinned to a named session.
type ExecRunner struct {
	Binary  string
	Session string
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if r.Session != "" {
		args = append([]string{"--session", r.Session}, args...)
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// Error is a structured herdr failure.
type Error struct {
	Code     string
	Message  string
	ExitCode int
	Args     []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("herdr %s: %s: %s", commandLabel(e.Args), e.Code, e.Message)
}

// commandLabel keeps the subcommand and target but drops payloads such as prompt text,
// which would otherwise flood logs and reports.
func commandLabel(args []string) string {
	var parts []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(parts) == 3 {
			break
		}
		if len(a) > 64 || strings.ContainsAny(a, "\n") {
			a = "<" + strconv.Itoa(len(a)) + " bytes>"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// Error codes: CodeTimeout comes from herdr (e.g. wait-output); the others are set by Client.
const (
	CodeTimeout = "timeout"
	codeUsage   = "usage"
	codeExec    = "exec"
)

// IsCode reports whether err is a herdr Error with the given code.
func IsCode(err error, code string) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == code
}

type Agent struct {
	Name        string `json:"name"`
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Cwd         string `json:"cwd"`
}

type Workspace struct {
	ID         string
	TabID      string
	RootPaneID string
}

type Tab struct {
	ID         string
	RootPaneID string
}

type Session struct {
	Name       string `json:"name"`
	Default    bool   `json:"default"`
	Running    bool   `json:"running"`
	SocketPath string `json:"socket_path"`
}

type Client struct {
	r Runner
}

func New(r Runner) *Client { return &Client{r: r} }

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	out, stderr, err := c.r.Run(ctx, args...)
	if err == nil {
		return out, nil
	}
	exitCode := -1
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		exitCode = ee.ExitCode()
	}
	// herdr documents errors on stderr; fall back to stdout to be tolerant.
	for _, b := range [][]byte{stderr, out} {
		var env struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(bytes.TrimSpace(b), &env) == nil && env.Error != nil {
			return nil, &Error{Code: env.Error.Code, Message: env.Error.Message, ExitCode: exitCode, Args: args}
		}
	}
	code := codeExec
	if exitCode == 2 {
		code = codeUsage
	}
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		msg = err.Error()
	}
	return nil, &Error{Code: code, Message: msg, ExitCode: exitCode, Args: args}
}

func (c *Client) runJSON(ctx context.Context, dst any, args ...string) error {
	out, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	// `pane run` and `send-keys` print nothing on success (verified in E2E); only a command
	// that must return data treats empty output as an error.
	if dst == nil && len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		return fmt.Errorf("herdr %s: decode response: %w", strings.Join(args, " "), err)
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, dst); err != nil {
		return fmt.Errorf("herdr %s: decode result: %w", strings.Join(args, " "), err)
	}
	return nil
}

// Sessions lists named herdr sessions (works without a running server).
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	out, err := c.run(ctx, "session", "list", "--json")
	if err != nil {
		return nil, err
	}
	var v struct {
		Sessions []Session `json:"sessions"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("herdr session list: %w", err)
	}
	return v.Sessions, nil
}

// WorkspaceCreate opens a workspace at cwd without stealing focus. herdr creates its first tab
// and root pane with it; env (KEY=VALUE) is set for the pane's shell.
func (c *Client) WorkspaceCreate(ctx context.Context, cwd, label string, env []string) (Workspace, error) {
	args := []string{"workspace", "create", "--cwd", cwd, "--no-focus"}
	if label != "" {
		args = append(args, "--label", label)
	}
	args = appendEnv(args, env)
	var res struct {
		Workspace struct {
			ID string `json:"workspace_id"`
		} `json:"workspace"`
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := c.runJSON(ctx, &res, args...); err != nil {
		return Workspace{}, err
	}
	if res.Workspace.ID == "" || res.RootPane.ID == "" {
		return Workspace{}, errors.New("herdr workspace create: response has no workspace or root pane")
	}
	return Workspace{ID: res.Workspace.ID, TabID: res.Tab.ID, RootPaneID: res.RootPane.ID}, nil
}

// TabCreate opens a labeled tab in workspace, with env (KEY=VALUE) set for its shell. It never
// passes --focus.
func (c *Client) TabCreate(ctx context.Context, workspace, cwd, label string, env []string) (Tab, error) {
	var res struct {
		Tab struct {
			ID string `json:"tab_id"`
		} `json:"tab"`
		RootPane struct {
			ID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := c.runJSON(ctx, &res, appendEnv([]string{"tab", "create", "--workspace", workspace, "--cwd", cwd, "--label", label}, env)...); err != nil {
		return Tab{}, err
	}
	if res.RootPane.ID == "" {
		return Tab{}, errors.New("herdr tab create: response has no root pane")
	}
	return Tab{ID: res.Tab.ID, RootPaneID: res.RootPane.ID}, nil
}

func (c *Client) TabRename(ctx context.Context, tab, label string) error {
	return c.runJSON(ctx, nil, "tab", "rename", tab, label)
}

func (c *Client) AgentList(ctx context.Context) ([]Agent, error) {
	var res struct {
		Agents []Agent `json:"agents"`
	}
	err := c.runJSON(ctx, &res, "agent", "list")
	return res.Agents, err
}

// PaneRun sends a command line followed by Enter.
func (c *Client) PaneRun(ctx context.Context, pane, command string) error {
	return c.runJSON(ctx, nil, "pane", "run", pane, command)
}

// WorkspaceClose closes a workspace created by delamain. It never passes --group.
func (c *Client) WorkspaceClose(ctx context.Context, workspaceID string) error {
	return c.runJSON(ctx, nil, "workspace", "close", workspaceID)
}

// WorkspaceReportMetadata shows display-only tokens in herdr's sidebar. herdr ignores an update
// whose seq is not greater than the last one it saw from the same source.
func (c *Client) WorkspaceReportMetadata(ctx context.Context, workspaceID, source string, seq int64, tokens map[string]string) error {
	args := []string{"workspace", "report-metadata", workspaceID, "--source", source, "--seq", strconv.FormatInt(seq, 10)}
	for _, k := range slices.Sorted(maps.Keys(tokens)) {
		args = append(args, "--token", k+"="+tokens[k])
	}
	return c.runJSON(ctx, nil, args...)
}

func appendEnv(args, env []string) []string {
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	return args
}
