// Package claude drives one Claude Code session as a subprocess in headless stream-json mode
// (`claude -p --input-format stream-json --output-format stream-json`): user messages as
// newline-delimited JSON on stdin, events on stdout, one `result` event per turn.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paraizo/delamain/internal/agent"
	"github.com/paraizo/delamain/internal/gitx"
)

const (
	maxEvent     = 64 << 20 // a tool result can be large; a longer line is dropped by the scanner
	defaultClose = 10 * time.Second
)

// abortGrace is how long an interrupted turn may take to end before claude is killed.
var abortGrace = 15 * time.Second

// StreamArgs put Claude Code in the multi-turn headless mode this client speaks.
var StreamArgs = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}

type Options struct {
	Binary string
	Dir    string
	Args   []string // claude arguments; StreamArgs are added
	// Env is appended to the inherited environment, already stripped of credentials by
	// gitx.AgentEnviron; its keys win over inherited ones.
	Env        []string
	Log        io.Writer // human-readable event log (what `delamain logs` and herdr show)
	Stderr     io.Writer
	Transcript io.Writer // every line sent and received, for audit
	// UsagePath receives the session's cumulative usage after every turn (see ReadUsage).
	UsagePath    string
	CloseTimeout time.Duration
}

// event is the subset of Claude Code's stream-json events this client reads.
type event struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	Model           string          `json:"model"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	// result
	IsError        bool                  `json:"is_error"`
	Result         string                `json:"result"`
	StopReason     string                `json:"stop_reason"`
	APIErrorStatus json.RawMessage       `json:"api_error_status"`
	Errors         []string              `json:"errors"`
	ModelUsage     map[string]modelUsage `json:"modelUsage"`
	// rate_limit_event
	RateLimit struct {
		Status        string `json:"status"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

type modelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type message struct {
	ID      string  `json:"id"`
	Model   string  `json:"model"`
	Content []block `json:"content"`
	Usage   struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // of tool_result: a string or text blocks
	IsError   bool            `json:"is_error"`
}

func (e event) msg() *message {
	var m message
	if len(e.Message) == 0 || e.Message[0] != '{' || json.Unmarshal(e.Message, &m) != nil {
		return nil
	}
	return &m
}

type Client struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	log        io.Writer
	transcript io.Writer
	usagePath  string
	closeIn    time.Duration
	stderr     *lastLine

	wmu sync.Mutex // serializes stdin and transcript writes
	tmu sync.Mutex // one turn at a time

	smu     sync.Mutex
	sink    chan event       // events for the turn in flight; nil between turns
	stop    chan struct{}    // closed when the turn stops listening
	total   agent.Usage      // cumulative usage at the last result, subagents included
	inTurn  map[string]usage // the turn's assistant messages so far, by message id
	tools   map[string]string
	model   string
	aborted bool
	turnN   int // counts prompts, so a late kill from Abort never hits a later turn

	done    chan struct{} // closed when stdout ends (claude exited)
	readErr error
	seq     int
	closed  sync.Once
	waited  sync.Once
	exited  chan struct{} // closed once the process is reaped
}

type usage = agent.Usage

// Start launches Claude Code. It sends nothing until the first prompt: a start-up failure
// (bad flag, no credentials) surfaces there, with claude's last stderr line.
func Start(_ context.Context, o Options) (*Client, error) {
	args := append(append([]string{}, StreamArgs...), o.Args...)
	cmd := exec.Command(o.Binary, args...) //nolint:gosec // configured binary
	cmd.Dir = o.Dir
	cmd.Env = append(gitx.AgentEnviron(os.Environ()), o.Env...) // exec keeps the last value of a duplicated key
	stderr := &lastLine{w: o.Stderr}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Close kills claude and its tools together
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	c := &Client{
		cmd: cmd, stdin: stdin, log: o.Log, transcript: o.Transcript, usagePath: o.UsagePath,
		closeIn: o.CloseTimeout, stderr: stderr, done: make(chan struct{}), exited: make(chan struct{}),
		inTurn: map[string]usage{}, tools: map[string]string{},
	}
	if c.log == nil {
		c.log = io.Discard
	}
	if c.transcript == nil {
		c.transcript = io.Discard
	}
	if c.closeIn <= 0 {
		c.closeIn = defaultClose
	}
	go c.read(stdout)
	return c, nil
}

// Spent is the session's usage, subagents included: the total at the last turn's end plus
// what this turn's assistant messages reported so far.
func (c *Client) Spent() agent.Usage {
	c.smu.Lock()
	defer c.smu.Unlock()
	u := c.total
	for _, m := range c.inTurn {
		u = u.Add(m)
	}
	return u
}

// PID is claude's process id, which is also its process group id.
func (c *Client) PID() int { return c.cmd.Process.Pid }

func (c *Client) read(stdout io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), maxEvent)
	for sc.Scan() {
		c.record(sc.Bytes())
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // stdout is protocol-only; a bad line is not worth killing the session
		}
		c.logEvent(e)
		c.smu.Lock()
		c.account(e)
		sink, stop := c.sink, c.stop
		c.smu.Unlock()
		if sink != nil {
			select {
			case sink <- e:
			case <-stop:
			}
		}
	}
	c.readErr = sc.Err()
}

// account folds usage in; called with smu held.
func (c *Client) account(e event) {
	switch e.Type {
	case "system":
		if e.Subtype == "init" && e.Model != "" {
			c.model = e.Model
		}
	case "assistant":
		// One response arrives as an event per content block, all with the same id and usage.
		if m := e.msg(); m != nil && m.ID != "" {
			c.inTurn[m.ID] = usage{
				Input: m.Usage.InputTokens, Output: m.Usage.OutputTokens,
				CacheRead: m.Usage.CacheReadInputTokens, CacheWrite: m.Usage.CacheCreationInputTokens,
			}
		}
	case "result":
		if len(e.ModelUsage) > 0 {
			c.total = sumModelUsage(e.ModelUsage)
		} else { // e.g. an interrupted turn: keep what its assistant messages reported
			for _, m := range c.inTurn {
				c.total = c.total.Add(m)
			}
		}
		c.inTurn = map[string]usage{}
	}
}

// sumModelUsage totals result.modelUsage, which is cumulative for the session and covers
// subagents, unlike result.usage.
func sumModelUsage(mu map[string]modelUsage) agent.Usage {
	var u agent.Usage
	for _, m := range mu {
		u = u.Add(agent.Usage{
			Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadInputTokens,
			CacheWrite: m.CacheCreationInputTokens, Cost: agent.Cost{Total: m.CostUSD},
		})
	}
	return u
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.transcript.Write(append(append([]byte{}, b...), '\n'))
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) record(line []byte) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = c.transcript.Write(append(append([]byte{}, line...), '\n'))
}

func (c *Client) nextID(prefix string) string {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	return fmt.Sprintf("%s%d", prefix, c.seq)
}

// Prompt sends text and waits for the turn's result. On timeout or ctx cancellation the turn
// is interrupted before returning.
func (c *Client) Prompt(ctx context.Context, text string, timeout time.Duration) (agent.Turn, error) {
	turn, err := c.prompt(ctx, text, timeout)
	// Every way out records the usage: a turn that timed out or died may be the costliest.
	if werr := c.writeUsage(); werr != nil && err == nil {
		err = fmt.Errorf("claude: gravar usage: %w", werr)
	}
	return turn, err
}

func (c *Client) prompt(ctx context.Context, text string, timeout time.Duration) (agent.Turn, error) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	sink, stop := make(chan event, 256), make(chan struct{})
	c.smu.Lock()
	c.sink, c.stop, c.aborted = sink, stop, false
	c.turnN++
	before := c.total
	c.smu.Unlock()
	defer func() {
		c.smu.Lock()
		c.sink, c.stop = nil, nil
		c.smu.Unlock()
		close(stop) // releases a reader blocked on a full channel
	}()

	c.logf("▶ prompt (%d caracteres)", len(text))
	msg := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	if err := c.send(msg); err != nil {
		return agent.Turn{}, fmt.Errorf("claude: enviar prompt: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	end := func(e event) (agent.Turn, error) {
		turn := c.turn(e, before)
		c.logf("■ fim do turno: %s · in %d out %d cacheW %d", turn.StopReason, turn.Usage.Input, turn.Usage.Output, turn.Usage.CacheWrite)
		return turn, nil
	}
	for {
		select {
		case e := <-sink:
			if e.Type == "result" {
				return end(e)
			}
		case <-c.done:
			// The reader may have queued the result just before claude exited.
			for len(sink) > 0 {
				if e := <-sink; e.Type == "result" {
					return end(e)
				}
			}
			if c.wasAborted() {
				return agent.Turn{StopReason: "aborted", Model: c.modelName()}, nil
			}
			return agent.Turn{}, c.exitError("claude saiu no meio do turno")
		case <-timer.C:
			return agent.Turn{}, c.interruptTurn(sink, agent.ErrTimeout)
		case <-ctx.Done():
			return agent.Turn{}, c.interruptTurn(sink, ctx.Err())
		}
	}
}

// turn maps a result event onto the pipeline's stop reasons.
func (c *Client) turn(e event, before agent.Usage) agent.Turn {
	c.smu.Lock()
	defer c.smu.Unlock()
	t := agent.Turn{Model: c.model, Usage: beyond(c.total, before), StopReason: e.StopReason}
	switch {
	case c.aborted:
		t.StopReason = "aborted"
	case e.IsError || e.Subtype != "success":
		t.StopReason, t.ErrorMessage = "error", resultError(e)
	case t.StopReason == "":
		t.StopReason = "stop"
	}
	return t
}

func resultError(e event) string {
	parts := []string{e.Subtype}
	if s := strings.TrimSpace(string(e.APIErrorStatus)); s != "" && s != "null" {
		parts = append(parts, "api "+s)
	}
	if r := strings.TrimSpace(e.Result); r != "" {
		parts = append(parts, r)
	}
	parts = append(parts, e.Errors...)
	return strings.Join(parts, ": ")
}

func beyond(u, o agent.Usage) agent.Usage {
	return agent.Usage{
		Input: u.Input - o.Input, Output: u.Output - o.Output, CacheRead: u.CacheRead - o.CacheRead,
		CacheWrite: u.CacheWrite - o.CacheWrite, Cost: agent.Cost{Total: u.Cost.Total - o.Cost.Total},
	}
}

// interruptTurn stops a turn the caller gave up on and waits briefly for its result, so the
// session is idle again; a session that ignores the interrupt is killed.
func (c *Client) interruptTurn(sink chan event, cause error) error {
	c.logf("✗ abortando o turno: %v", cause)
	_ = c.interrupt()
	grace := time.NewTimer(abortGrace)
	defer grace.Stop()
	for {
		select {
		case e := <-sink:
			if e.Type == "result" {
				return cause
			}
		case <-c.done:
			return cause
		case <-grace.C:
			c.kill()
			return fmt.Errorf("%w (e o interrupt não confirmou em %s)", cause, abortGrace)
		}
	}
}

// Abort interrupts the turn in flight; Prompt then returns it with StopReason "aborted". A
// session that does not end the turn within the grace period is killed.
func (c *Client) Abort() error {
	c.smu.Lock()
	n := c.turnN
	c.smu.Unlock()
	err := c.interrupt()
	grace := abortGrace
	go func() {
		select {
		case <-c.done:
		case <-time.After(grace):
			if c.turnInFlight(n) {
				c.kill()
			}
		}
	}()
	return err
}

func (c *Client) interrupt() error {
	c.smu.Lock()
	c.aborted = c.sink != nil
	c.smu.Unlock()
	return c.send(map[string]any{
		"type": "control_request", "request_id": c.nextID("i"),
		"request": map[string]string{"subtype": "interrupt"},
	})
}

// turnInFlight reports whether turn n is still running.
func (c *Client) turnInFlight(n int) bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.sink != nil && c.turnN == n
}

func (c *Client) wasAborted() bool {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.aborted
}

func (c *Client) modelName() string {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.model
}

func (c *Client) kill() { _ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL) }

// Close ends the session: EOF on stdin first, then the whole process group is killed.
func (c *Client) Close() error {
	c.closed.Do(func() {
		_ = c.stdin.Close()
		c.wait()
		select {
		case <-c.exited:
		case <-time.After(c.closeIn):
			c.kill()
			<-c.exited
		}
		// Tools claude started may outlive it in the group.
		c.kill()
	})
	return nil
}

// wait reaps the process in the background, once; exited closes when it is done.
func (c *Client) wait() {
	c.waited.Do(func() {
		go func() { _ = c.cmd.Wait(); close(c.exited) }()
	})
}

func (c *Client) exitError(what string) error {
	<-c.done
	c.wait()
	code := any("?")
	select {
	case <-c.exited: // also flushes stderr into lastLine
		code = c.cmd.ProcessState.ExitCode()
	case <-time.After(c.closeIn):
	}
	err := fmt.Errorf("%s (exit %v)", what, code)
	if l := c.stderr.last(); l != "" {
		err = fmt.Errorf("%w: %s", err, agent.Truncate(l, 300))
	}
	return errors.Join(err, c.readErr)
}

// writeUsage replaces the usage file with the session's cumulative usage, including a turn
// that ended without a result.
func (c *Client) writeUsage() error {
	if c.usagePath == "" {
		return nil
	}
	b, err := json.Marshal(c.Spent())
	if err != nil {
		return err
	}
	tmp := c.usagePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.usagePath)
}

// UsageSuffix names the usage file of a session: <session id>.usage.json.
const UsageSuffix = ".usage.json"

// ReadUsage reads a usage file written after a turn.
func ReadUsage(path string) (agent.Usage, error) {
	var u agent.Usage
	b, err := os.ReadFile(path) //nolint:gosec // under the run's sessions dir
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return u, fmt.Errorf("%s: %w", path, err)
	}
	return u, nil
}

// UsageFiles lists the usage files under a run's sessions dir (layout <role>/<id>.usage.json).
func UsageFiles(sessionsDir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(sessionsDir, "*", "*"+UsageSuffix))
	sort.Strings(files)
	return files, err
}

func (c *Client) logf(format string, a ...any) {
	fmt.Fprintf(c.log, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// lastLine passes stderr through and keeps its last non-empty line for error messages.
type lastLine struct {
	w    io.Writer
	mu   sync.Mutex
	line string
	part string
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	lines := strings.Split(l.part+string(p), "\n")
	l.part = lines[len(lines)-1]
	for _, s := range lines[:len(lines)-1] {
		if s = strings.TrimSpace(s); s != "" {
			l.line = s
		}
	}
	l.mu.Unlock()
	if l.w == nil {
		return len(p), nil
	}
	return l.w.Write(p)
}

func (l *lastLine) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := strings.TrimSpace(l.part); s != "" {
		return s
	}
	return l.line
}
