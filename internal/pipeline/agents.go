package pipeline

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/paraizo/delamain/internal/agent"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/claude"
	"github.com/paraizo/delamain/internal/omp/rpc"
	"github.com/paraizo/delamain/internal/store"
)

// Agents starts agent sessions on one harness (omp, claude). The pipeline drives them
// directly; herdr only shows their logs.
type Agents interface {
	Start(ctx context.Context, o AgentOptions) (Agent, error)
	// Sessions reads the usage the harness recorded under a run's sessions dir.
	Sessions(sessionsDir string) ([]budget.SessionUsage, error)
}

// Agent is one running agent session.
type Agent interface {
	// Prompt runs one turn to its end; on timeout the turn is aborted and agent.ErrTimeout returned.
	Prompt(ctx context.Context, text string, timeout time.Duration) (agent.Turn, error)
	// Abort stops the turn in flight, which then ends with StopReason "aborted".
	Abort() error
	// Spent is the session's usage as reported live, ahead of what the harness has written to disk.
	Spent() agent.Usage
	// PID is the harness' process id, recorded so other delamain processes can tell it is alive.
	PID() int
	Close() error
}

type AgentOptions struct {
	Name         string // e.g. implementer-r7, for logs
	Dir          string
	SessionDir   string   // where the harness keeps this role's session files
	SystemPrompt string   // path of the role body appended to the harness' system prompt
	Env          []string // wins over the inherited environment
	LogPath      string   // human-readable event log, read by `delamain logs` (and a herdr tab)
	ErrPath      string   // the harness' stderr
	// Confine, when set, restricts a session that reads third-party code (a PR review).
	Confine *Confinement
}

// Confinement lets an agent read Checkout, run read-only git there (diff, log, show) and write
// only Artifact: the code it reads is untrusted, so its harness must not hand it a free shell.
type Confinement struct {
	Checkout string
	Artifact string
}

// RPCAgents runs each session as `omp --mode rpc`.
type RPCAgents struct {
	Binary                        string
	Model, Thinking, ApprovalMode string
}

func (a RPCAgents) args(o AgentOptions) []string {
	args := []string{
		"--model", a.Model, "--thinking", a.Thinking, "--approval-mode", a.ApprovalMode,
		"--session-dir", o.SessionDir, "--append-system-prompt", o.SystemPrompt,
	}
	if o.Confine != nil {
		// omp has no per-command allowlist: a confined session only loses LSP, which could run
		// the checkout's build scripts.
		args = append(args, "--no-lsp")
	}
	return args
}

func (RPCAgents) Sessions(sessionsDir string) ([]budget.SessionUsage, error) {
	return budget.OmpSessions(sessionsDir)
}

func (a RPCAgents) Start(ctx context.Context, o AgentOptions) (Agent, error) {
	logf, err := os.OpenFile(o.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	errf, err := os.OpenFile(o.ErrPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = logf.Close()
		return nil, err
	}
	c, err := rpc.Start(ctx, rpc.Options{
		Binary: a.Binary, Dir: o.Dir, Args: a.args(o), Env: o.Env,
		Log: logf, Stderr: errf, ReadyTimeout: startTimeout,
	})
	if err != nil {
		_, _ = logf.Close(), errf.Close()
		return nil, err
	}
	return &rpcAgent{Client: c, files: []*os.File{logf, errf}}, nil
}

type rpcAgent struct {
	*rpc.Client
	files []*os.File
}

func (a *rpcAgent) Close() error {
	err := a.Client.Close()
	for _, f := range a.files {
		err = errors.Join(err, f.Close())
	}
	return err
}

// ClaudeAgents runs each session as Claude Code in headless stream-json mode.
type ClaudeAgents struct {
	Binary                        string
	Model, Effort, PermissionMode string
	AgentsFile, PluginDir         string // delamain's subagents and skills (`delamain prompts install`)
}

func (a ClaudeAgents) args(sessionID, rolePrompt string, c *Confinement) []string {
	mode := a.PermissionMode
	if c != nil {
		mode = "dontAsk" // anything not allowed by confined() is denied, never prompted
	}
	args := []string{
		"--model", a.Model, "--effort", a.Effort, "--permission-mode", mode,
		"--session-id", sessionID, "--no-session-persistence",
		"--append-system-prompt", rolePrompt,
		"--agents", a.AgentsFile, "--plugin-dir", a.PluginDir,
		// Unattended and apart from the user's own Claude Code setup: no dialog may block a
		// turn, and only the target repository's project settings apply.
		"--setting-sources", "project", "--strict-mcp-config",
		"--permission-prompts", "none", "--disallowed-tools", "AskUserQuestion",
	}
	if c != nil {
		args = append(args, confined(c)...)
	}
	// Commits carry no Co-Authored-By trailer, as with omp.
	return append(args, "--settings", `{"attribution":{"commit":"","pr":"","sessionUrl":false}}`)
}

// confined narrows Claude Code to reading the checkout, read-only git in it and writing the
// artifact. Rules on absolute paths start with "//"; Edit rules cover Write. Subagents inherit
// them. Verified against Claude Code 2.1.303: reads elsewhere, other commands (compound ones
// included) and other writes are denied.
func confined(c *Confinement) []string {
	git := "Bash(git -C " + c.Checkout + " "
	return []string{
		"--tools", "Read,Grep,Glob,Bash,Write,Agent",
		"--allowedTools", "Read(/" + c.Checkout + "/**)", git + "diff:*)", git + "log:*)", git + "show:*)", "Edit(/" + c.Artifact + ")",
	}
}

func (ClaudeAgents) Sessions(sessionsDir string) ([]budget.SessionUsage, error) {
	files, err := claude.UsageFiles(sessionsDir)
	if err != nil {
		return nil, err
	}
	out := make([]budget.SessionUsage, 0, len(files))
	for _, f := range files {
		u, err := claude.ReadUsage(f)
		if err != nil {
			return nil, err
		}
		out = append(out, budget.SessionUsage{Role: filepath.Base(filepath.Dir(f)), File: f, Usage: storeUsage(u)})
	}
	return out, nil
}

func (a ClaudeAgents) Start(ctx context.Context, o AgentOptions) (Agent, error) {
	role, err := os.ReadFile(o.SystemPrompt)
	if err != nil {
		return nil, err
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	var files []*os.File
	closeAll := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, p := range []string{o.LogPath, o.ErrPath, filepath.Join(o.SessionDir, id+".stream.jsonl")} {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // run dir paths
		if err != nil {
			closeAll()
			return nil, err
		}
		files = append(files, f)
	}
	c, err := claude.Start(ctx, claude.Options{
		Binary: a.Binary, Dir: o.Dir, Args: a.args(id, string(role), o.Confine), Env: o.Env,
		Log: files[0], Stderr: files[1], Transcript: files[2],
		UsagePath: filepath.Join(o.SessionDir, id+claude.UsageSuffix),
	})
	if err != nil {
		closeAll()
		return nil, err
	}
	return &claudeAgent{Client: c, files: files}, nil
}

type claudeAgent struct {
	*claude.Client
	files []*os.File
}

func (a *claudeAgent) Close() error {
	err := a.Client.Close()
	for _, f := range a.files {
		err = errors.Join(err, f.Close())
	}
	return err
}

// newSessionID is a random UUID (version 4), which is what claude's --session-id takes.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func storeUsage(u agent.Usage) store.Usage {
	return store.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.Cost.Total}
}
