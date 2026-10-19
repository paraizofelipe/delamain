package forge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
