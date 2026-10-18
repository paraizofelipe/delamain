package rpc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Contract tests against the real omp. They spend a few thousand LLM tokens:
//
//	DELAMAIN_IT_OMP_MODEL=openai-codex/gpt-5.6-terra go test ./internal/omp/rpc/ -run Integration
func integrationClient(t *testing.T) *Client {
	t.Helper()
	model := os.Getenv("DELAMAIN_IT_OMP_MODEL")
	if model == "" {
		t.Skip("set DELAMAIN_IT_OMP_MODEL to run omp RPC integration tests")
	}
	c, err := Start(context.Background(), Options{
		Binary: "omp", Dir: t.TempDir(), Log: &syncBuffer{},
		Args: []string{"--no-session", "--model", model, "--thinking", "low", "--approval-mode", "yolo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestIntegrationShouldCompleteATurnAndReportUsage(t *testing.T) {
	c := integrationClient(t)
	turn, err := c.Prompt(context.Background(), "Responda apenas: ok", 2*time.Minute)
	if err != nil || turn.StopReason != "stop" || turn.Usage.Input == 0 {
		t.Fatalf("got %+v %v", turn, err)
	}
}

func TestIntegrationShouldAbortALongTool(t *testing.T) {
	c := integrationClient(t)
	go func() {
		time.Sleep(8 * time.Second)
		_ = c.Abort()
	}()
	began := time.Now()
	turn, err := c.Prompt(context.Background(), "Rode no bash exatamente `sleep 120` e depois responda FIM", 2*time.Minute)
	if err != nil || turn.StopReason != "aborted" || time.Since(began) > time.Minute {
		t.Fatalf("got %+v %v after %s", turn, err, time.Since(began))
	}
	if turn, err = c.Prompt(context.Background(), "Responda apenas: vivo", 2*time.Minute); err != nil || !strings.EqualFold(strings.TrimSpace(turn.StopReason), "stop") {
		t.Fatalf("the session must stay usable after an abort: %+v %v", turn, err)
	}
}
