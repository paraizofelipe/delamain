package herdr

import (
	"context"
	"os"
	"testing"
	"time"
)

// Integration tests run against a real, isolated herdr session and spend no LLM tokens:
//
//	herdr --session delamain-test server &
//	DELAMAIN_IT_HERDR_SESSION=delamain-test go test ./internal/herdr/ -run Integration
func integrationClient(t *testing.T) *Client {
	t.Helper()
	session := os.Getenv("DELAMAIN_IT_HERDR_SESSION")
	if session == "" {
		t.Skip("set DELAMAIN_IT_HERDR_SESSION to run herdr integration tests")
	}
	return New(ExecRunner{Binary: "herdr", Session: session})
}

func TestIntegrationShouldReadState(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := c.AgentList(ctx); err != nil {
		t.Fatalf("agent list: %v", err)
	}
}
