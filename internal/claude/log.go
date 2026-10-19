package claude

import (
	"encoding/json"
	"strings"

	"github.com/paraizo/delamain/internal/agent"
)

// logEvent writes the human-readable line of an event, in the format the omp client uses, so
// `delamain logs` and herdr read the same log whatever the harness.
func (c *Client) logEvent(e event) {
	sub := ""
	if e.ParentToolUseID != nil && *e.ParentToolUseID != "" {
		sub = "⑂ " // a subagent's own work
	}
	switch e.Type {
	case "assistant":
		m := e.msg()
		if m == nil {
			return
		}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					c.logf("%s💬 %s", sub, agent.Truncate(t, 2000))
				}
			case "tool_use":
				c.rememberTool(b.ID, b.Name)
				if b.Name == "Agent" || b.Name == "Task" {
					c.logf("⑂ subagent %s: %s", field(b.Input, "subagent_type"), agent.Truncate(field(b.Input, "description"), 160))
					continue
				}
				c.logf("%s🔧 %s: %s", sub, b.Name, toolDetail(b.Input))
			}
		}
	case "user":
		m := e.msg()
		if m == nil {
			return
		}
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.IsError {
				c.logf("%s✗ %s falhou%s", sub, c.toolName(b.ToolUseID), agent.ToolFailure(resultText(b.Content)))
			}
		}
	case "result":
		if e.IsError || e.Subtype != "success" {
			c.logf("✗ %s", agent.Truncate(resultError(e), 500))
		}
	case "rate_limit_event":
		if s := e.RateLimit.Status; s != "" && s != "allowed" {
			c.logf("⚠ rate limit %s: %s", e.RateLimit.RateLimitType, s)
		}
	}
}

func (c *Client) rememberTool(id, name string) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.tools[id] = name
}

func (c *Client) toolName(id string) string {
	c.smu.Lock()
	defer c.smu.Unlock()
	if n := c.tools[id]; n != "" {
		return n
	}
	return "tool"
}

// toolDetail picks what a tool call is about: its description, command, path or pattern.
func toolDetail(input json.RawMessage) string {
	for _, k := range []string{"description", "command", "file_path", "pattern", "path", "skill"} {
		if v := field(input, k); v != "" {
			return agent.Truncate(v, 160)
		}
	}
	return agent.Truncate(string(input), 160)
}

func field(input json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// resultText is a tool_result's content: a string or a list of text blocks.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []block
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
