package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Option configures the HTTP client New builds.
type Option func(*options)

type options struct {
	httpClient *http.Client
	baseURL    string
	backoff    time.Duration
	userAgent  string
}

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// WithBaseURL points github.com or gitlab.com at another API root; meant for tests.
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// WithBackoff sets the wait before the first retry of a transient failure.
func WithBackoff(d time.Duration) Option { return func(o *options) { o.backoff = d } }

// WithUserAgent sets the User-Agent sent on every request.
func WithUserAgent(ua string) Option { return func(o *options) { o.userAgent = ua } }

func newOptions(opts []Option) options {
	o := options{backoff: 2 * time.Second, userAgent: "delamain"}
	for _, opt := range opts {
		opt(&o)
	}
	if o.httpClient == nil {
		o.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return o
}

// APIError is a request the forge answered with an error status. Message is already
// truncated and stripped of credentials.
type APIError struct {
	Forge   Kind
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Forge, e.Op, e.Status, e.Message)
}

// opError is a request that got no answer from the forge (network, timeout, decoding).
type opError struct {
	msg string
	err error
}

func (e *opError) Error() string { return e.msg }
func (e *opError) Unwrap() error { return e.err }

// caller holds what every request of a client shares.
type caller struct {
	kind    Kind
	token   string // scrubbed from every error
	backoff time.Duration
}

const maxErrorLen = 500

// fail builds the error of a failed attempt: an *APIError when the forge answered. The bodies
// the attempt sent are hidden before the message is cut, so no part of them survives.
func (c caller) fail(op string, status int, msg string, err error, bodies []string) error {
	if status >= 400 {
		return &APIError{Forge: c.kind, Op: op, Status: status, Message: truncate(hide(redact(strings.TrimSpace(msg), c.token), bodies), maxErrorLen)}
	}
	return &opError{msg: fmt.Sprintf("%s %s: %s", c.kind, op, truncate(hide(redact(err.Error(), c.token), bodies), maxErrorLen)), err: err}
}

const attempts = 3

// retry runs try up to attempts times while it reports a transient failure. It waits the
// backoff, doubling it each time, or the server's Retry-After when given, and never past ctx.
func (c caller) retry(ctx context.Context, try func() (transient bool, after time.Duration, err error)) error {
	wait := c.backoff
	for i := 1; ; i++ {
		transient, after, err := try()
		if err == nil || !transient || i == attempts || ctx.Err() != nil {
			return err
		}
		d := wait
		if after > 0 {
			d = after
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		wait *= 2
	}
}

// transient says whether a failed attempt is worth repeating: a gateway error, or no answer
// at all because of the network. API rejections (4xx, 500) are final.
func transient(ctx context.Context, status int, err error) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case 0:
		_, network := errors.AsType[*url.Error](err)
		return network && ctx.Err() == nil
	}
	return false
}

// createdAnyway handles a CreatePR that failed with exists (GitHub 422, GitLab 409) after a
// retry: the earlier attempt reached the forge before its answer was lost, so the PR/MR of head
// is returned instead of the error.
func createdAnyway(ctx context.Context, f interface {
	OpenPR(context.Context, string) (*PR, error)
}, head string, tries, exists int, err error) (PR, error) {
	var apiErr *APIError
	if tries < 2 || !errors.As(err, &apiErr) || apiErr.Status != exists {
		return PR{}, err
	}
	pr, openErr := f.OpenPR(ctx, head)
	if openErr != nil || pr == nil {
		return PR{}, err
	}
	return *pr, nil
}

// hide stars out of s the bodies a write sent, as is or JSON-escaped: a forge may quote what
// it rejected, and bodies never reach errors or logs.
func hide(s string, bodies []string) string {
	for _, b := range bodies {
		if b == "" {
			continue
		}
		s = strings.ReplaceAll(s, b, "***")
		var esc strings.Builder
		enc := json.NewEncoder(&esc)
		enc.SetEscapeHTML(false)
		if enc.Encode(b) == nil {
			quoted := strings.TrimSpace(esc.String())
			s = strings.ReplaceAll(s, quoted[1:len(quoted)-1], "***")
		}
	}
	return s
}

// truncate cuts s to n characters, never inside a UTF-8 sequence.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// New returns the forge client for repo, authenticated with token.
func New(repo Repo, token string, opts ...Option) (Forge, error) {
	o := newOptions(opts)
	if repo.Kind == GitLab {
		return newGitLab(repo, token, o)
	}
	return newGitHub(repo, token, o)
}

// Whoami returns the login token authenticates as on repo's forge (GET /user).
func Whoami(ctx context.Context, repo Repo, token string, opts ...Option) (string, error) {
	o := newOptions(opts)
	if repo.Kind == GitLab {
		g, err := newGitLab(repo, token, o)
		if err != nil {
			return "", err
		}
		return g.whoami(ctx)
	}
	g, err := newGitHub(repo, token, o)
	if err != nil {
		return "", err
	}
	return g.whoami(ctx)
}
