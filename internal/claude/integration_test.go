package claude

import (
	"context"
	"os"
	"testing"
	"time"
)

// Contract tests against the real Claude Code. They spend a few thousand LLM tokens:
//
//	DELAMAIN_IT_CLAUDE_MODEL=haiku go test ./internal/claude/ -run Integration
func integrationClient(t *testing.T) *Client {
	t.Helper()
	log := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("claude log:\n%s", log.String())
		}
	})
	model := os.Getenv("DELAMAIN_IT_CLAUDE_MODEL")
	if model == "" {
		t.Skip("set DELAMAIN_IT_CLAUDE_MODEL to run Claude Code integration tests")
	}
	c, err := Start(context.Background(), Options{
		Binary: "claude", Dir: t.TempDir(), Log: log,
		Args: []string{
			"--model", model, "--effort", "low", "--permission-mode", "bypassPermissions", "--no-session-persistence",
			"--setting-sources", "project", "--strict-mcp-config", "--permission-prompts", "none",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestIntegrationShouldCompleteATurnAndReportUsage(t *testing.T) {
	c := integrationClient(t)
	turn, err := c.Prompt(context.Background(), "Reply with exactly: ok", 2*time.Minute)
	if err != nil || turn.StopReason != "end_turn" {
		t.Fatalf("got %+v %v", turn, err)
	}
	if u := c.Spent(); u.Input+u.CacheRead+u.CacheWrite == 0 || u.Output == 0 || u.Cost.Total == 0 {
		t.Fatalf("usage must be reported: %+v", u)
	}
}

func TestIntegrationShouldInterruptALongToolAndKeepTheSession(t *testing.T) {
	c := integrationClient(t)
	go func() {
		time.Sleep(12 * time.Second)
		_ = c.Abort()
	}()
	began := time.Now()
	turn, err := c.Prompt(context.Background(), "Use the Bash tool to run exactly `python3 -c 'import time; time.sleep(120)'` in the foreground (not in the background), wait for it to finish, then reply DONE", 2*time.Minute)
	if err != nil || turn.StopReason != "aborted" || time.Since(began) > time.Minute {
		t.Fatalf("got %+v %v after %s", turn, err, time.Since(began))
	}
	// Alive means the interrupt worked: a session that ignored it would have been killed.
	if turn, err = c.Prompt(context.Background(), "Reply with exactly: alive", 2*time.Minute); err != nil || turn.StopReason != "end_turn" {
		t.Fatalf("the session must stay usable after an interrupt: %+v %v", turn, err)
	}
}
