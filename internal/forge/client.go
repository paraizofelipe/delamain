package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"
	gl "gitlab.com/gitlab-org/api/client-go/v3"
)

// errEmpty is a successful status without the expected body.
var errEmpty = errors.New("forge answered without a body")

// attempts is how many times a transient failure is tried before it is reported.
const attempts = 3

// maxRetryAfter caps how long a GitHub secondary rate limit is waited out; longer waits fail
// the call so the daemon is not stuck for an hour.
const maxRetryAfter = 60 * time.Second

// transientStatus reports gateway errors worth retrying; API rejections are not.
func transientStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// retryTransport retries GitHub calls on network errors, gateway errors and the secondary rate
// limit, bounding each attempt by timeout. GitLab gets the same policy from client-go's retrier.
type retryTransport struct {
	base    http.RoundTripper
	backoff time.Duration
	timeout time.Duration
}

func (t retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	wait := t.backoff
	for i := 1; ; i++ {
		resp, err := t.attempt(req, i)
		delay, retry := t.retryAfter(resp, err, wait)
		if !retry || i == attempts || req.Context().Err() != nil {
			return resp, err
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(delay):
		}
		wait *= 2
	}
}

// attempt sends one try of req, with a fresh body after the first.
func (t retryTransport) attempt(req *http.Request, i int) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(req.Context(), t.timeout)
	r := req.Clone(ctx)
	if i > 1 && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			cancel()
			return nil, err
		}
		r.Body = body
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = cancelOnClose{resp.Body, cancel} // the attempt's deadline covers reading the body
	return resp, nil
}

// retryAfter says whether the outcome of an attempt is worth another one, and after how long.
func (t retryTransport) retryAfter(resp *http.Response, err error, wait time.Duration) (time.Duration, bool) {
	if err != nil {
		return wait, true
	}
	if transientStatus(resp.StatusCode) {
		return wait, true
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		secs, perr := strconv.Atoi(s)
		if perr != nil || time.Duration(secs)*time.Second > maxRetryAfter {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	// Peek at the body for the secondary limit message, then hand it back intact.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	return wait, strings.Contains(strings.ToLower(string(b)), "secondary rate limit")
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// gitlabRetry is client-go's retry policy narrowed to network and gateway errors.
func gitlabRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return err != nil || transientStatus(resp.StatusCode), nil
}

// maxMessage bounds the API message an error carries.
const maxMessage = 500

// apiError turns a client library error into an *APIError whose message never holds the token.
func apiError(kind Kind, op string, scrub func(string) string, err error) error {
	if err == nil {
		return nil
	}
	e := &APIError{Forge: kind, Op: op, err: err}
	var (
		ghErr    *gh.ErrorResponse
		ghAbuse  *gh.AbuseRateLimitError
		ghRate   *gh.RateLimitError
		glErr    *gl.ErrorResponse
		response *http.Response
	)
	switch {
	case errors.As(err, &ghErr):
		response, e.Message = ghErr.Response, ghErr.Message
	case errors.As(err, &ghAbuse):
		response, e.Message = ghAbuse.Response, ghAbuse.Message
	case errors.As(err, &ghRate):
		response, e.Message = ghRate.Response, ghRate.Message
	case errors.As(err, &glErr):
		e.Status, e.Message = glErr.StatusCode, glMessage(glErr)
	default:
		e.Message = err.Error()
	}
	if response != nil {
		e.Status = response.StatusCode
	}
	e.Message = truncate(scrub(e.Message), maxMessage)
	return e
}

// glMessage prefers the API's own message over client-go's rendering of the whole body.
func glMessage(e *gl.ErrorResponse) string {
	var body struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(e.Body, &body) == nil {
		if m, ok := body.Message.(string); ok && m != "" {
			return m
		}
		if body.Error != "" {
			return body.Error
		}
	}
	return e.Message
}

var userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)

// scrubber returns a func that removes token and URL credentials from text bound for errors.
func scrubber(token string) func(string) string {
	return func(s string) string {
		if token != "" {
			s = strings.ReplaceAll(s, token, "[redacted]")
		}
		return userinfo.ReplaceAllString(s, "$1")
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
