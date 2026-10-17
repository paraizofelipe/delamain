package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSocket serves one connection: reads the subscribe request, writes ack then lines.
func fakeSocket(t *testing.T, ack string, lines ...string) (path string, gotReq <-chan map[string]any) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs") // short path: unix socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	reqc := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		if sc.Scan() {
			var req map[string]any
			_ = json.Unmarshal(sc.Bytes(), &req)
			reqc <- req
		}
		conn.Write([]byte(ack + "\n"))
		for _, l := range lines {
			conn.Write([]byte(l + "\n"))
		}
		time.Sleep(200 * time.Millisecond)
	}()
	return path, reqc
}

func TestSubscribeShouldDeliverAgentStatusEventsInOrder(t *testing.T) {
	// Lines captured from herdr 0.9.3 during the M0 spike.
	path, reqc := fakeSocket(t, `{"id":"delamain-sub","result":{"type":"subscription_started"}}`,
		`{"data":{"agent":"omp","agent_status":"working","pane_id":"w2:p1","workspace_id":"w2"},"event":"pane.agent_status_changed"}`,
		`not json`,
		`{"data":{"agent":"omp","agent_status":"done","pane_id":"w2:p1","workspace_id":"w2"},"event":"pane.agent_status_changed"}`,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	events, _, err := Subscribe(ctx, path, []Subscription{{Type: "pane.agent_status_changed", PaneID: "w2:p1"}})
	if err != nil {
		t.Fatal(err)
	}
	req := <-reqc
	if req["method"] != "events.subscribe" {
		t.Fatalf("wrong method: %v", req)
	}
	var statuses []string
	for ev := range events {
		var d AgentStatusChange
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, d.AgentStatus)
		if len(statuses) == 2 {
			cancel()
		}
	}
	if len(statuses) != 2 || statuses[0] != StatusWorking || statuses[1] != StatusDone {
		t.Fatalf("malformed lines must be skipped and order kept, got %v", statuses)
	}
}

func TestSubscribeShouldFailWhenServerRejectsSubscription(t *testing.T) {
	path, _ := fakeSocket(t, `{"id":"delamain-sub","error":{"code":"invalid_request","message":"missing field pane_id"}}`)
	_, _, err := Subscribe(context.Background(), path, []Subscription{{Type: "pane.agent_status_changed"}})
	if !IsCode(err, "invalid_request") {
		t.Fatalf("want invalid_request, got %v", err)
	}
}

func TestSubscribeShouldFailWhenSocketIsMissing(t *testing.T) {
	_, _, err := Subscribe(context.Background(), filepath.Join(t.TempDir(), "absent.sock"), nil)
	if err == nil {
		t.Fatal("dialing a missing socket must fail")
	}
}
