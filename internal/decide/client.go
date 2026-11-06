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

	defaultTimeout = 30 * time.Second // used when Options.Timeout is not positive, so a stalled server never hangs the caller
)

var errBodyTooLarge = errors.New("response body exceeds 1 MiB")

// Options configure a Client; the caller resolves them (the package reads no config).
type Options struct {
	BaseURL string // e.g. https://openrouter.ai/api
	APIKey  string
	Model   string
	Timeout time.Duration // zero or negative means 30s
	HTTP    *http.Client  // optional; default &http.Client{Timeout: Timeout}
}

// Client asks the OpenRouter Decisions API. It never retries.
type Client struct {
	opts Options
	http *http.Client
}

// New builds a Client from resolved options.
func New(o Options) *Client {
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
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
	Noul          *float64           `json:"noul"`
	Choice        *string            `json:"choice"`
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
		return Result{}, c.fail("encode state", err)
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
		return Result{}, c.fail("encode request", err)
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
	if resp.StatusCode/100 != 2 {
		// A body that fails to read must not hide the status: keep whatever arrived and the cause.
		return Result{}, &APIError{Status: resp.StatusCode, Message: truncate(c.redact(strings.TrimSpace(string(raw))), maxErrorBytes), cause: err}
	}
	if err != nil {
		return Result{}, c.fail("read response", err)
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
	for name := range req.Nouls {
		a, err := c.noul(name, v.Answers)
		if err != nil {
			return Result{}, err
		}
		res.Answers[name] = a
	}
	for name, ch := range req.Choices {
		a, err := c.choice(name, ch, v.Answers)
		if err != nil {
			return Result{}, err
		}
		res.Answers[name] = a
	}
	return res, nil
}

// APIError is a non-2xx answer of the Decisions API.
type APIError struct {
	Status  int
	Message string // response body, without the API key, cut to 300 bytes

	cause error // why reading the body failed, if it did
}

func (e *APIError) Error() string { return fmt.Sprintf("decisions: %d %s", e.Status, e.Message) }
func (e *APIError) Unwrap() error { return e.cause }

// noul checks the answer to the noul name: present, a noul, and a probability in [0,1].
func (c *Client) noul(name string, answers map[string]answer) (Answer, error) {
	a, ok := answers[name]
	switch {
	case !ok:
		return Answer{}, c.invalid(name, "no answer")
	case a.Type != "noul":
		return Answer{}, c.invalid(name, fmt.Sprintf("answered as %q, want noul", a.Type))
	case a.Noul == nil:
		return Answer{}, c.invalid(name, "noul has no value")
	case *a.Noul < 0 || *a.Noul > 1:
		return Answer{}, c.invalid(name, fmt.Sprintf("noul %v is outside [0,1]", *a.Noul))
	}
	return Answer{Noul: *a.Noul}, nil
}

// choice checks the answer to the choice name: present, a choice, one of the options sent,
// with confidence and probabilities in [0,1] and keyed by those options.
func (c *Client) choice(name string, ch Choice, answers map[string]answer) (Answer, error) {
	a, ok := answers[name]
	switch {
	case !ok:
		return Answer{}, c.invalid(name, "no answer")
	case a.Type != "choice":
		return Answer{}, c.invalid(name, fmt.Sprintf("answered as %q, want choice", a.Type))
	case a.Choice == nil:
		return Answer{}, c.invalid(name, "choice has no value")
	}
	if _, ok := ch.Options[*a.Choice]; !ok {
		return Answer{}, c.invalid(name, fmt.Sprintf("choice %q is not among the options", *a.Choice))
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		return Answer{}, c.invalid(name, fmt.Sprintf("confidence %v is outside [0,1]", a.Confidence))
	}
	for opt, p := range a.Probabilities {
		if _, ok := ch.Options[opt]; !ok {
			return Answer{}, c.invalid(name, fmt.Sprintf("probability for %q, which is not among the options", opt))
		}
		if p < 0 || p > 1 {
			return Answer{}, c.invalid(name, fmt.Sprintf("probability %v of %q is outside [0,1]", p, opt))
		}
	}
	return Answer{Choice: *a.Choice, Confidence: a.Confidence, Probabilities: a.Probabilities}, nil
}

// invalid is the error of an answer that breaks the request it answers.
func (c *Client) invalid(question, reason string) error {
	return fmt.Errorf("invalid decision answer: %s: %s", question, c.redact(reason))
}

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
