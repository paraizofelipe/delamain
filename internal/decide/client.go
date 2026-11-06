package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes caps how much of a response is read.
const maxBodyBytes = 1 << 20

// Options configure a Client; the caller resolves them (the package reads no config).
type Options struct {
	BaseURL string // e.g. https://openrouter.ai/api
	APIKey  string
	Model   string
	Timeout time.Duration
	HTTP    *http.Client // optional; default &http.Client{Timeout: Timeout}
}

// Client asks the OpenRouter Decisions API. It never retries.
type Client struct {
	opts Options
	http *http.Client
}

// New builds a Client from resolved options.
func New(o Options) *Client {
	h := o.HTTP
	if h == nil {
		h = &http.Client{Timeout: o.Timeout}
	}
	return &Client{opts: o, http: h}
}

// question is one entry of the request "questions" object.
type question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

// answer is one entry of the response "answers" object.
type answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Decide sends req to POST {BaseURL}/alpha/decisions and returns the validated answers.
func (c *Client) Decide(ctx context.Context, req Request) (Result, error) {
	questions := map[string]question{}
	for name, n := range req.Nouls {
		questions[name] = question{Type: "noul", Instructions: n.Instructions, Criteria: map[string]string{"true": n.True, "false": n.False}}
	}
	for name, ch := range req.Choices {
		questions[name] = question{Type: "choice", Instructions: ch.Instructions, Criteria: ch.Options}
	}
	body, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     map[string]any      `json:"state"`
		Questions map[string]question `json:"questions"`
	}{c.opts.Model, req.State, questions})
	if err != nil {
		return Result{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.opts.BaseURL, "/")+"/alpha/decisions", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.http.Do(hreq)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var v struct {
		Model   string            `json:"model"`
		Answers map[string]answer `json:"answers"`
		Usage   struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&v); err != nil {
		return Result{}, err
	}
	res := Result{Answers: map[string]Answer{}, Model: v.Model, CostUSD: v.Usage.Cost, Latency: time.Since(start)}
	for name, a := range v.Answers {
		res.Answers[name] = Answer{Noul: a.Noul, Choice: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities}
	}
	return res, nil
}
