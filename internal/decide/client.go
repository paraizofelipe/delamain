package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxStateBytes = 32000   // above it the model falls into its slower, less precise long-context mode
	maxBodyBytes  = 1 << 20 // caps how much of a response is read
	maxErrorBytes = 300
)

var errBodyTooLarge = errors.New("response body exceeds 1 MiB")

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
	if len(req.Nouls)+len(req.Choices) == 0 {
		return Result{}, ErrNoQuestions
	}
	state, err := json.Marshal(req.State)
	if err != nil {
		return Result{}, fmt.Errorf("decision state: %w", err)
	}
	if len(state) > maxStateBytes {
		return Result{}, ErrStateTooLarge
	}
	questions := map[string]question{}
	for name, n := range req.Nouls {
		questions[name] = question{Type: "noul", Instructions: n.Instructions, Criteria: map[string]string{"true": n.True, "false": n.False}}
	}
	for name, ch := range req.Choices {
		if _, dup := questions[name]; dup {
			return Result{}, fmt.Errorf("decision question %q is both a noul and a choice", name)
		}
		questions[name] = question{Type: "choice", Instructions: ch.Instructions, Criteria: ch.Options}
	}
	body, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     json.RawMessage     `json:"state"`
		Questions map[string]question `json:"questions"`
	}{c.opts.Model, state, questions})
	if err != nil {
		return Result{}, err
	}
	if c.opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.Timeout)
		defer cancel()
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.opts.BaseURL, "/")+"/alpha/decisions", bytes.NewReader(body))
	if err != nil {
		return Result{}, c.fail("build request", err)
	}
	hreq.Header.Set("Authorization", "Bearer "+c.opts.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.http.Do(hreq)
	if err != nil {
		return Result{}, c.fail("send request", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Result{}, c.fail("read response", err)
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, &APIError{Status: resp.StatusCode, Message: truncate(c.redact(strings.TrimSpace(string(raw))), maxErrorBytes)}
	}
	if len(raw) > maxBodyBytes {
		return Result{}, c.fail("read response", errBodyTooLarge)
	}
	var v struct {
		Model   string            `json:"model"`
		Answers map[string]answer `json:"answers"`
		Usage   struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return Result{}, c.fail("decode response", err)
	}
	res := Result{Answers: map[string]Answer{}, Model: v.Model, CostUSD: v.Usage.Cost, Latency: time.Since(start)}
	for name, a := range v.Answers {
		res.Answers[name] = Answer{Noul: a.Noul, Choice: a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities}
	}
	return res, nil
}

// APIError is a non-2xx answer of the Decisions API.
type APIError struct {
	Status  int
	Message string // response body, without the API key, cut to 300 bytes
}

func (e *APIError) Error() string { return fmt.Sprintf("decisions: %d %s", e.Status, e.Message) }

// opError is a failure that got no answer from the API (request, network, timeout, decoding).
type opError struct {
	msg string
	err error
}

func (e *opError) Error() string { return e.msg }
func (e *opError) Unwrap() error { return e.err }

// fail wraps err for op, without the API key, keeping err reachable for errors.Is.
func (c *Client) fail(op string, err error) error {
	return &opError{msg: "decisions: " + op + ": " + c.redact(err.Error()), err: err}
}

// redact replaces every occurrence of the API key so it never reaches an error.
func (c *Client) redact(s string) string {
	if c.opts.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.opts.APIKey, "[redacted]")
}

// truncate cuts s to at most n bytes, never inside a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
