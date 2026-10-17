package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// Subscription selects one event stream. Pane events require PaneID (verified in M0).
type Subscription struct {
	Type        string `json:"type"`
	PaneID      string `json:"pane_id,omitempty"`
	AgentStatus string `json:"agent_status,omitempty"`
}

// Event is one pushed line: {"event": "<type>", "data": {...}}.
type Event struct {
	Type string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

// AgentStatusChange is the data of pane.agent_status_changed.
type AgentStatusChange struct {
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
}

// SocketPath resolves the API socket of a session ("" means the default session).
func (c *Client) SocketPath(ctx context.Context, session string) (string, error) {
	sessions, err := c.Sessions(ctx)
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if (session == "" && s.Default) || (session != "" && s.Name == session) {
			if !s.Running {
				return "", fmt.Errorf("herdr session %q is not running", s.Name)
			}
			return s.SocketPath, nil
		}
	}
	return "", fmt.Errorf("herdr session %q not found", session)
}

// Subscribe opens the event stream. The channel closes when ctx ends or the socket closes;
// the error channel receives at most one terminal error.
func Subscribe(ctx context.Context, socketPath string, subs []Subscription) (<-chan Event, <-chan error, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("dial herdr socket: %w", err)
	}
	req := map[string]any{
		"id":     "delamain-sub",
		"method": "events.subscribe",
		"params": map[string]any{"subscriptions": subs},
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("write subscribe: %w", err)
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	if !sc.Scan() {
		conn.Close()
		cause := sc.Err()
		if cause == nil {
			cause = io.EOF
		}
		return nil, nil, fmt.Errorf("subscribe: no acknowledgement: %w", cause)
	}
	var ack struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(sc.Bytes(), &ack); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("subscribe: decode ack: %w", err)
	}
	if ack.Error != nil {
		conn.Close()
		return nil, nil, &Error{Code: ack.Error.Code, Message: ack.Error.Message, Args: []string{"events.subscribe"}}
	}

	events := make(chan Event)
	errc := make(chan error, 1)
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	go func() {
		defer close(events)
		for sc.Scan() {
			var ev Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil || ev.Type == "" {
				continue // tolerate unknown lines for forward compatibility
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
			errc <- err
		}
	}()
	return events, errc, nil
}
