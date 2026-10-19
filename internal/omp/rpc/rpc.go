// Package rpc drives one omp session as a subprocess over its RPC mode (`omp --mode rpc`):
// newline-delimited JSON commands on stdin, responses and events on stdout.
// Protocol: https://omp.sh/docs/rpc. Only protocol v1 is used.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paraizo/delamain/internal/agent"
	"github.com/paraizo/delamain/internal/gitx"
)

// ErrTimeout means a turn did not end in time; it was aborted.
var ErrTimeout = agent.ErrTimeout

const (
	maxFrame     = 1 << 20 // omp's advertised maxFrameBytes in protocol v1
	abortGrace   = 15 * time.Second
	defaultReady = 30 * time.Second
	defaultClose = 10 * time.Second
)

type Options struct {
	Binary string
	Dir    string
	Args   []string // omp arguments; "--mode rpc" is added
	// Env is appended to the inherited environment; its keys win over inherited ones.
	Env          []string
	Log          io.Writer // human-readable event log (what the herdr tab shows)
	Stderr       io.Writer
	ReadyTimeout time.Duration
	CloseTimeout time.Duration
}

type (
	Usage = agent.Usage
	Cost  = agent.Cost
	// Turn is the outcome of one prompt, taken from its last assistant message. StopReason is
	// empty when no assistant message arrived (e.g. a locally handled command or a dropped frame).
	Turn = agent.Turn
)

type message struct {
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        Usage           `json:"usage"`
	Content      json.RawMessage `json:"content"`
}

type frame struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Command      string          `json:"command"`
	Success      *bool           `json:"success"`
	Error        string          `json:"error"`
	IsTerminal   *bool           `json:"isTerminal"`
	AgentInvoked *bool           `json:"agentInvoked"`
	Data         json.RawMessage `json:"data"`
	Message      json.RawMessage `json:"message"` // an AgentMessage, or a string on notice
	Method       string          `json:"method"`
	ToolName     string          `json:"toolName"`
	Intent       string          `json:"intent"`
	Args         json.RawMessage `json:"args"`
	IsError      bool            `json:"isError"`
	Result       struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"` // of tool_execution_end
	Level        string `json:"level"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	ErrorMessage string `json:"errorMessage"`
	Payload      struct {
		Agent  string `json:"agent"`
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"payload"`
}

type sink struct {
	ch   chan frame
	stop chan struct{} // closed when the turn stops listening
}

// msg decodes the frame's AgentMessage, or returns nil.
func (f frame) msg() *message {
	var m message
	if len(f.Message) == 0 || f.Message[0] != '{' || json.Unmarshal(f.Message, &m) != nil {
		return nil
	}
	return &m
}

type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	log     io.Writer
	closeIn time.Duration

	wmu   sync.Mutex // serializes stdin writes
	tmu   sync.Mutex // one turn at a time
	smu   sync.Mutex
	sink  *sink // frames for the turn in flight; nil between turns
	spent Usage // every assistant message so far, subagents included

	done    chan struct{} // closed when stdout ends (omp exited)
	readErr error
	seq     int
	closed  sync.Once
}

// Start launches omp in RPC mode and waits for its ready frame.
func Start(ctx context.Context, o Options) (*Client, error) {
	cmd := exec.Command(o.Binary, append([]string{"--mode", "rpc"}, o.Args...)...) //nolint:gosec // configured binary
	cmd.Dir = o.Dir
	// No forge credential reaches the agent; exec keeps the last value of a duplicated key.
	cmd.Env = append(gitx.AgentEnviron(os.Environ()), o.Env...)
	cmd.Stderr = o.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Close kills omp and its tools together
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("omp: %w", err)
	}
	c := &Client{cmd: cmd, stdin: stdin, log: o.Log, closeIn: o.CloseTimeout, done: make(chan struct{})}
	if c.log == nil {
		c.log = io.Discard
	}
	if c.closeIn <= 0 {
		c.closeIn = defaultClose
	}
	ready := make(chan struct{})
	go c.read(stdout, ready)

	wait := o.ReadyTimeout
	if wait <= 0 {
		wait = defaultReady
	}
	select {
	case <-ready:
		return c, nil
	case <-c.done:
		_ = c.Close()
		return nil, errors.Join(fmt.Errorf("omp saiu antes de ficar pronto (exit %v)", c.exitCode()), c.readErr)
	case <-time.After(wait):
		_ = c.Close()
		return nil, fmt.Errorf("omp não ficou pronto em %s", wait)
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

// Spent is the usage of every assistant message the session reported, subagents' included.
// It is live, unlike the session JSONL, which omp writes in batches.
func (c *Client) Spent() Usage {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.spent
}

// PID is omp's process id, which is also its process group id.
func (c *Client) PID() int { return c.cmd.Process.Pid }

func (c *Client) read(stdout io.Reader, ready chan struct{}) {
	defer close(c.done)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), maxFrame+1024)
	isReady := false
	for sc.Scan() {
		var f frame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue // stdout is protocol-only; a bad line is not worth killing the session
		}
		switch f.Type {
		case "ready":
			if !isReady {
				isReady = true
				close(ready)
			}
			continue
		case "extension_ui_request":
			c.answerUI(f)
			continue
		}
		c.logFrame(f)
		c.smu.Lock()
		if m := f.msg(); f.Type == "message_end" && m != nil && m.Role == "assistant" {
			c.spent = c.spent.Add(m.Usage)
		}
		s := c.sink
		c.smu.Unlock()
		if s != nil {
			select {
			case s.ch <- f:
			case <-s.stop:
			}
		}
	}
	c.readErr = sc.Err()
}

// answerUI cancels dialogs: an unattended run has nobody to answer, and omp blocks until it
// gets a reply. Presentation-only methods take none.
func (c *Client) answerUI(f frame) {
	switch f.Method {
	case "select", "confirm", "input", "editor":
		c.logf("⚠ diálogo cancelado (%s)", f.Method)
		_ = c.send(map[string]any{"type": "extension_ui_response", "id": f.ID, "cancelled": true})
	}
}

func (c *Client) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

func (c *Client) nextID(prefix string) string {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	return fmt.Sprintf("%s%d", prefix, c.seq)
}

// Prompt sends text and waits until the turn ends. On timeout or ctx cancellation the turn
// is aborted before returning.
func (c *Client) Prompt(ctx context.Context, text string, timeout time.Duration) (Turn, error) {
	c.tmu.Lock()
	defer c.tmu.Unlock()
	s := &sink{ch: make(chan frame, 256), stop: make(chan struct{})}
	c.smu.Lock()
	c.sink = s
	c.smu.Unlock()
	defer func() {
		c.smu.Lock()
		c.sink = nil
		c.smu.Unlock()
		close(s.stop) // releases a reader blocked on a full channel
	}()
	sink := s.ch

	id := c.nextID("p")
	c.logf("▶ prompt (%d caracteres)", len(text))
	if err := c.send(map[string]any{"id": id, "type": "prompt", "message": text}); err != nil {
		return Turn{}, fmt.Errorf("omp: enviar prompt: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var turn Turn
	for {
		select {
		case f := <-sink:
			if end, err := apply(&turn, f, id); end {
				if err == nil {
					c.logf("■ fim do turno: %s · in %d out %d cacheW %d", orDash(turn.StopReason), turn.Usage.Input, turn.Usage.Output, turn.Usage.CacheWrite)
				}
				return turn, err
			}
		case <-c.done:
			return turn, fmt.Errorf("omp saiu no meio do turno (exit %v)", c.exitCode())
		case <-timer.C:
			return turn, c.abortTurn(sink, ErrTimeout)
		case <-ctx.Done():
			return turn, c.abortTurn(sink, ctx.Err())
		}
	}
}

// apply folds one frame into the turn and reports whether the turn ended.
func apply(t *Turn, f frame, id string) (bool, error) {
	switch {
	case f.Type == "response" && f.ID == id && f.Success != nil && !*f.Success:
		// A scheduled prompt can fail after its ack, with no agent_end at all.
		return true, fmt.Errorf("omp recusou o prompt: %s", f.Error)
	case f.Type == "response" && f.ID == id && notInvoked(f.Data):
		return true, nil
	case f.Type == "prompt_result" && f.ID == id && f.AgentInvoked != nil && !*f.AgentInvoked:
		return true, nil
	case f.Type == "message_end" && f.msg() != nil && f.msg().Role == "assistant":
		m := f.msg()
		*t = Turn{StopReason: m.StopReason, ErrorMessage: m.ErrorMessage, Model: m.Model, Usage: m.Usage}
	case f.Type == "agent_end" && (f.IsTerminal == nil || *f.IsTerminal):
		return true, nil
	}
	return false, nil
}

func notInvoked(data json.RawMessage) bool {
	var d struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	return json.Unmarshal(data, &d) == nil && d.AgentInvoked != nil && !*d.AgentInvoked
}

func (c *Client) abortTurn(sink chan frame, cause error) error {
	c.logf("✗ abortando o turno: %v", cause)
	_ = c.Abort()
	grace := time.NewTimer(abortGrace)
	defer grace.Stop()
	for {
		select {
		case f := <-sink:
			if f.Type == "agent_end" && (f.IsTerminal == nil || *f.IsTerminal) {
				return cause
			}
		case <-c.done:
			return cause
		case <-grace.C:
			return fmt.Errorf("%w (e o abort não confirmou em %s)", cause, abortGrace)
		}
	}
}

// Abort stops the turn in flight; Prompt then returns it with StopReason "aborted".
func (c *Client) Abort() error {
	return c.send(map[string]any{"id": c.nextID("a"), "type": "abort"})
}

// Close ends the session: EOF on stdin first, then the whole process group is killed.
func (c *Client) Close() error {
	c.closed.Do(func() {
		_ = c.stdin.Close()
		exited := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(exited) }()
		select {
		case <-exited:
		case <-time.After(c.closeIn):
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		// Tools omp started may outlive it in the group.
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	})
	return nil
}

func (c *Client) exitCode() any {
	if c.cmd.ProcessState != nil {
		return c.cmd.ProcessState.ExitCode()
	}
	return "?"
}

func (c *Client) logf(format string, a ...any) {
	fmt.Fprintf(c.log, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

func (c *Client) logFrame(f frame) {
	switch f.Type {
	case "tool_execution_start":
		detail := f.Intent
		if detail == "" {
			detail = agent.Truncate(string(f.Args), 160)
		}
		c.logf("🔧 %s: %s", f.ToolName, detail)
	case "tool_execution_end":
		if f.IsError {
			c.logf("✗ %s falhou%s", f.ToolName, agent.ToolFailure(text(f.Result.Content)))
		}
	case "message_end":
		m := f.msg()
		if m == nil || m.Role != "assistant" {
			return
		}
		if txt := text(m.Content); txt != "" {
			c.logf("💬 %s", agent.Truncate(txt, 2000))
		}
		if m.ErrorMessage != "" {
			c.logf("✗ %s: %s", m.StopReason, agent.Truncate(m.ErrorMessage, 500))
		}
	case "subagent_lifecycle":
		c.logf("⑂ subagent %s (%s): %s", f.Payload.ID, f.Payload.Agent, f.Payload.Status)
	case "notice":
		var notice string
		_ = json.Unmarshal(f.Message, &notice)
		c.logf("⚠ %s %s", f.Level, agent.Truncate(notice, 500))
	case "auto_retry_start":
		c.logf("↻ retry %d/%d: %s", f.Attempt, f.MaxAttempts, agent.Truncate(f.ErrorMessage, 300))
	case "rpc_frame_error", "extension_error":
		c.logf("✗ %s: %s", f.Type, agent.Truncate(f.Error, 300))
	}
}

func text(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
