package claude

import (
	"encoding/json"
	"strings"
	"unicode"

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
	case "system":
		if e.Subtype == "hook_response" {
			c.logHook(sub, e)
		}
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
				if action, ok := hindsightAction(b.Name); ok {
					c.logf("%s🧠 %s", sub, hindsightLine(action, b.Input))
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

// logHook reports Hindsight's recall hook, told apart by its tags; other hooks may be the target
// repo's and are not logged. Only the injected size goes out, never the memory itself.
func (c *Client) logHook(sub string, e event) {
	ctx := hookContext(e.Output)
	recall := strings.Contains(ctx, "<hindsight_memory>")
	if !recall && !strings.Contains(ctx, "<hindsight_knowledge_refresh>") {
		return
	}
	switch {
	case e.ExitCode != nil && *e.ExitCode != 0:
		c.logf("%s✗ recall automático falhou (exit %d)", sub, *e.ExitCode)
	case recall:
		c.logf("%s🧠 recall automático: %d caracteres injetados", sub, len(ctx))
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

// hindsightAction is a Hindsight MCP tool's name without its prefixes: search_knowledge_pages for
// mcp__hindsight__hindsight_search_knowledge_pages.
func hindsightAction(tool string) (string, bool) {
	name, ok := strings.CutPrefix(tool, "mcp__hindsight__")
	return strings.TrimPrefix(name, "hindsight_"), ok
}

// hindsightLine says what a Hindsight tool call is about by its query, title or page, never by
// the memory it writes (content, summary).
func hindsightLine(action string, input json.RawMessage) string {
	switch action {
	case "search_knowledge_pages":
		return labeled("busca nas páginas", quoted(field(input, "query")))
	case "read_knowledge_page":
		return labeled("lendo página", oneLine(field(input, "page_id")))
	case "list_knowledge_pages":
		return "listando páginas"
	case "reflect":
		return labeled("reflect", quoted(field(input, "query")))
	case "ingest_document":
		return labeled("retain", quoted(field(input, "title")))
	case "capture_initiative":
		return labeled("iniciativa registrada", quoted(field(input, "title")))
	}
	return oneLine(action)
}

func labeled(label, detail string) string {
	if detail == "" {
		return label
	}
	return label + ": " + detail
}

func quoted(s string) string {
	if s = oneLine(s); s == "" {
		return ""
	}
	return `"` + s + `"`
}

// oneLine truncates s with its control characters blanked, so a model-chosen value can neither
// forge log lines nor drive the terminal that shows them.
func oneLine(s string) string {
	return agent.Truncate(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s), 160)
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

// hookContext is what a hook injected: its JSON output's additionalContext, or its raw stdout.
func hookContext(output string) string {
	var o struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(output), &o) == nil && o.HookSpecificOutput.AdditionalContext != "" {
		return o.HookSpecificOutput.AdditionalContext
	}
	return output
}
