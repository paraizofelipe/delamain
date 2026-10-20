package forge

import (
	"fmt"
	"net/http"
	"strings"
	"time"
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
		o.httpClient = &http.Client{}
	}
	return o
}

// APIError is a request the forge answered with an error status.
type APIError struct {
	Forge   Kind
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Forge, e.Op, e.Status, strings.TrimSpace(e.Message))
}

// New returns the forge client for repo, authenticated with token.
func New(repo Repo, token string, opts ...Option) (Forge, error) {
	o := newOptions(opts)
	if repo.Kind == GitLab {
		return newGitLab(repo, token, o)
	}
	return newGitHub(repo, token, o)
}
