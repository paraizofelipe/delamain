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
func integrationClient(t *testing.T) (*Client, string) {
	t.Helper()
	session := os.Getenv("DELAMAIN_IT_HERDR_SESSION")
	if session == "" {
		t.Skip("set DELAMAIN_IT_HERDR_SESSION to run herdr integration tests")
	}
	return New(ExecRunner{Binary: "herdr", Session: session}), session
}

func TestIntegrationShouldResolveSocketAndReadState(t *testing.T) {
	c, session := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sock, err := c.SocketPath(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket %s: %v", sock, err)
	}
	if st, err := c.IntegrationStatus(ctx, "omp"); err != nil || st == "" {
		t.Fatalf("integration status: %q %v", st, err)
	}
	if _, err := c.AgentList(ctx); err != nil {
		t.Fatalf("agent list: %v", err)
	}
	if _, err := c.AgentGet(ctx, "delamain-it-absent"); !IsCode(err, CodeAgentNotFound) {
		t.Fatalf("real server must return agent_not_found, got %v", err)
	}
}

func TestIntegrationShouldAcknowledgeEventSubscription(t *testing.T) {
	c, session := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sock, err := c.SocketPath(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Subscribe(ctx, sock, []Subscription{{Type: "workspace.created"}}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, _, err := Subscribe(ctx, sock, []Subscription{{Type: "pane.agent_status_changed"}}); !IsCode(err, "invalid_request") {
		t.Fatalf("pane events without pane_id must be rejected by the server (M0 finding), got %v", err)
	}
}
