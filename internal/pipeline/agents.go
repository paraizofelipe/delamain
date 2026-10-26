package pipeline

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	// Model and Reasoning (omp's --thinking, claude's --effort) override the harness defaults
	// for this role; empty keeps them.
	Model, Reasoning string
}

// Confinement lets an agent read Checkout, run read-only git there (diff, log, show) and write
// only Artifact: the code it reads is untrusted, so its harness must not hand it a free shell.
type Confinement struct {
	Checkout string
	Base     string // the commit the PR is diffed against
	Artifact string
	Fix      bool
	// CommitMessage is the file a Fix session writes each commit message to.
	CommitMessage string
}

// RPCAgents runs each session as `omp --mode rpc`.
type RPCAgents struct {
	Binary                        string
	Model, Thinking, ApprovalMode string
}

func (a RPCAgents) args(o AgentOptions) []string {
	return []string{
		"--model", cmp.Or(o.Model, a.Model), "--thinking", cmp.Or(o.Reasoning, a.Thinking), "--approval-mode", a.ApprovalMode,
		"--session-dir", o.SessionDir, "--append-system-prompt", o.SystemPrompt,
	}
}

func (RPCAgents) Sessions(sessionsDir string) ([]budget.SessionUsage, error) {
	return budget.OmpSessions(sessionsDir)
}

func (a RPCAgents) Start(ctx context.Context, o AgentOptions) (Agent, error) {
	if o.Confine != nil {
		return nil, errors.New("omp cannot confine a session: it has no per-command allowlist, so the agent would get a free shell")
	}
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
	BinaryArgs                    []string // a launcher's subcommand between Binary and the arguments
	Model, Effort, PermissionMode string
	AgentsFile, PluginDir         string   // delamain's subagents and skills (`delamain prompts install`)
	Env                           []string // "K=V" entries appended to every session's environment
	Settings                      map[string]any
}

func (a ClaudeAgents) args(sessionID, rolePrompt string, o AgentOptions) []string {
	c := o.Confine
	mode := a.PermissionMode
	if c != nil {
		mode = "dontAsk" // anything not allowed by confined() is denied, never prompted
	}
	args := []string{
		"--model", cmp.Or(o.Model, a.Model), "--effort", cmp.Or(o.Reasoning, a.Effort), "--permission-mode", mode,
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
	// The configured settings (claude.settings) merged under delamain's policy: commits carry
	// no Co-Authored-By trailer, as with omp. Validate() vouched the map encodes as JSON.
	settings := map[string]any{}
	maps.Copy(settings, a.Settings)
	settings["attribution"] = map[string]any{"commit": "", "pr": "", "sessionUrl": false}
	b, err := json.Marshal(settings)
	if err != nil {
		// Validate() vouched claude.settings encodes; should a map ever get here without it,
		// the attribution policy still stands.
		b = []byte(`{"attribution":{"commit":"","pr":"","sessionUrl":false}}`)
	}
	return append(args, "--settings", string(b))
}

// confined narrows Claude Code to reading the checkout, two exact git commands in it and writing
// the artifact. A Fix session may also edit the checkout, never its .git, and stage and commit
// there, all exact commands: the commit message comes from the CommitMessage file the session
// writes, since a prefix rule ("commit -m:*") would also take --pathspec-from-file=<any file> (read
// any file) or -F <any file>. The commit runs no hook: a hooksPath inside the checkout would let
// the edits run code. It still runs nothing it edits. Verified against Claude Code 2.1.305:
// writes in .git, push, extra arguments and other commands are denied. The git rules are whole commands, not prefixes: "diff:*" would also allow
// --no-index (read any file) and --output=<path> (write any file). The diff runs no external diff
// nor textconv, whose driver the checkout's .gitattributes picks. Rules on absolute paths start
// with "//"; Edit rules cover Write; subagents inherit them. Verified against Claude Code 2.1.303:
// reads elsewhere (symlinks out of the checkout included), extra flags or arguments, compound
// commands and other writes are denied.
func confined(c *Confinement) []string {
	git := "Bash(git -C " + c.Checkout + " "
	allowed := []string{
		"Read(/" + c.Checkout + "/**)",
		git + "diff --no-ext-diff --no-textconv " + c.Base + "...HEAD)", git + "log " + c.Base + "..HEAD --oneline)",
		"Edit(/" + c.Artifact + ")",
	}
	if !c.Fix {
		return append([]string{"--tools", "Read,Grep,Glob,Bash,Write,Agent", "--allowedTools"}, allowed...)
	}
	allowed = append(allowed, "Edit(/"+c.Checkout+"/**)", git+"status --porcelain)", git+"add -A)",
		"Edit(/"+c.CommitMessage+")", git+"-c core.hooksPath=/dev/null commit -F "+c.CommitMessage+")")
	args := append([]string{"--tools", "Read,Grep,Glob,Bash,Edit,Write,Agent", "--allowedTools"}, allowed...)
	return append(args, "--disallowed-tools", "AskUserQuestion", "Edit(/"+c.Checkout+"/.git)", "Edit(/"+c.Checkout+"/.git/**)")
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
		Binary: a.Binary, BinaryArgs: a.BinaryArgs, Dir: o.Dir, Args: a.args(id, string(role), o),
		// The configured env first: the role's own env (the push block) wins on a clash.
		Env: append(append([]string{}, a.Env...), o.Env...),
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
