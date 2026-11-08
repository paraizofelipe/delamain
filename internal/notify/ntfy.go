package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/paraizo/delamain/internal/config"
)

// ntfyBackend publishes each toast to an ntfy topic, so a phone subscribed to it gets a push.
type ntfyBackend struct {
	client *http.Client
	url    string
	topic  string
	token  string // "" publishes without auth
	prio   config.NtfyPriority
}

// ntfyPublish is ntfy's JSON publish body (POST to the server root).
type ntfyPublish struct {
	Topic    string       `json:"topic"`
	Title    string       `json:"title,omitempty"`
	Message  string       `json:"message"`
	Priority int          `json:"priority"`
	Tags     []string     `json:"tags,omitempty"`
	Click    string       `json:"click,omitempty"`
	Actions  []ntfyAction `json:"actions,omitempty"`
}

// ntfyAction is one button under the notification; ntfy shows at most three.
type ntfyAction struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	URL    string `json:"url"`
}

const (
	ntfyMaxActions = 3
	// ntfyBodyLimit bounds the error body kept from a refused publish.
	ntfyBodyLimit = 4 << 10
)

func newNtfy(cfg config.Ntfy, token string, client *http.Client) *ntfyBackend {
	if client == nil {
		client = &http.Client{}
	}
	return &ntfyBackend{client: client, url: strings.TrimRight(cfg.URL, "/") + "/", topic: cfg.Topic, token: token, prio: cfg.Priority}
}

// send publishes m as JSON: the header form of ntfy is ASCII only, and the titles carry emoji.
func (n *ntfyBackend) send(ctx context.Context, m Message) error {
	body, err := json.Marshal(n.publish(m))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
	res, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode/100 == 2 {
		return nil
	}
	reason, _ := io.ReadAll(io.LimitReader(res.Body, 200))
	return fmt.Errorf("ntfy: %s: %s", res.Status, strings.TrimSpace(string(reason)))
}

func (n *ntfyBackend) close() error { return nil }

// publish maps a toast to ntfy: the default https action is the click, the other https ones
// become buttons, and local targets (a report file) mean nothing on a phone.
func (n *ntfyBackend) publish(m Message) ntfyPublish {
	p := ntfyPublish{
		Topic: n.topic, Title: m.Title, Message: m.Body, Priority: n.priority(m.Urgency),
		Tags: []string{"delamain", string(m.Event)},
	}
	seen := map[string]bool{}
	for _, a := range m.Actions {
		if !httpsURL(a.Target) || seen[a.Target] {
			continue
		}
		seen[a.Target] = true
		if a.Key == "default" && p.Click == "" {
			p.Click = a.Target
			continue
		}
		if len(p.Actions) < ntfyMaxActions {
			p.Actions = append(p.Actions, ntfyAction{Action: "view", Label: a.Label, URL: a.Target})
		}
	}
	return p
}

func (n *ntfyBackend) priority(u Urgency) int {
	switch u {
	case Low:
		return n.prio.Low
	case Critical:
		return n.prio.Critical
	default:
		return n.prio.Normal
	}
}

func httpsURL(target string) bool {
	u, err := url.Parse(target)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// NtfyProbe is what `delamain doctor` reports about the ntfy push.
type NtfyProbe struct {
	URL   string
	Topic string // masked: the first characters only
	Auth  bool   // a token is set
	Err   error  // why the server did not answer the health check
}

// diagnoseNtfy asks the server's health endpoint, with the token when there is one.
func diagnoseNtfy(ctx context.Context, cfg config.Ntfy, s sys) *NtfyProbe {
	if !cfg.Enabled {
		return nil
	}
	token := ""
	if cfg.TokenEnv != "" {
		token = s.getenv(cfg.TokenEnv)
	}
	p := &NtfyProbe{URL: cfg.URL, Topic: maskTopic(cfg.Topic), Auth: token != ""}
	client := s.http
	if client == nil {
		client = &http.Client{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.URL, "/")+"/v1/health", nil)
	if err != nil {
		p.Err = err
		return p
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		p.Err = err
		return p
	}
	defer func() { _ = res.Body.Close() }()
	var health struct {
		Healthy bool `json:"healthy"`
	}
	if res.StatusCode/100 != 2 {
		p.Err = fmt.Errorf("health: %s", res.Status)
	} else if err := json.NewDecoder(io.LimitReader(res.Body, ntfyBodyLimit)).Decode(&health); err != nil {
		p.Err = fmt.Errorf("health: %w", err)
	} else if !health.Healthy {
		p.Err = errors.New("health: the server reports itself unhealthy")
	}
	return p
}

// maskTopic keeps the first three characters of a topic, which is the secret of a public server.
func maskTopic(topic string) string {
	r := []rune(topic)
	if len(r) <= 3 {
		return topic
	}
	return string(r[:3]) + "…"
}
