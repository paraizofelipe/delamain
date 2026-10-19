// Package forge opens PRs (GitHub) and MRs (GitLab) through their REST APIs and manages issue
// labels and comments. It deliberately has no merge operation: delamain stops at the PR/MR.
package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// Repo identifies a project on a forge. It never carries credentials.
type Repo struct {
	Kind Kind
	Host string
	Path string // owner/repo or group/subgroup/repo
}

func (r Repo) String() string { return r.Host + "/" + r.Path }

type Issue struct {
	Number int
	Title  string
	Body   string
	URL    string
	Labels []string
	Open   bool
}

// PR is a GitHub pull request or a GitLab merge request.
type PR struct {
	Number int
	URL    string
	Draft  bool
}

type PROptions struct {
	Head, Base  string
	Title, Body string
	Draft       bool
}

// Forge is everything delamain does on a forge. There is intentionally no Merge.
type Forge interface {
	Repo() Repo
	Issue(ctx context.Context, number int) (Issue, error)
	// ListReady returns open issues carrying label, oldest (lowest number) first.
	ListReady(ctx context.Context, label string) ([]Issue, error)
	// OpenPR returns the open PR/MR whose source is head, or nil.
	OpenPR(ctx context.Context, head string) (*PR, error)
	CreatePR(ctx context.Context, o PROptions) (PR, error)
	Comment(ctx context.Context, number int, body string) error
	Relabel(ctx context.Context, number int, add, remove []string) error
}

// APIError is a failed forge call. Message is redacted and truncated; Status is 0 when no
// HTTP response came back.
type APIError struct {
	Forge   Kind
	Op      string
	Status  int
	Message string
	err     error
}

func (e *APIError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("%s %s: %s", e.Forge, e.Op, e.Message)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Forge, e.Op, e.Status, e.Message)
}

func (e *APIError) Unwrap() error { return e.err }

// Option tunes the API client New builds.
type Option func(*options)

type options struct {
	httpClient *http.Client
	baseURL    string
	userAgent  string
	backoff    time.Duration
	timeout    time.Duration
}

// WithHTTPClient sets the HTTP client the API calls go through.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// WithBaseURL overrides the API base URL derived from the repository host.
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// WithBackoff sets the wait before the first retry of a transient failure; it doubles after.
func WithBackoff(d time.Duration) Option { return func(o *options) { o.backoff = d } }

// WithTimeout bounds each HTTP attempt, 30s by default.
func WithTimeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// WithUserAgent sets the User-Agent sent to the forge, "delamain/dev" by default.
func WithUserAgent(ua string) Option { return func(o *options) { o.userAgent = ua } }

// New returns the API client of repo's forge, authenticated with token.
func New(repo Repo, token string, opts ...Option) (Forge, error) {
	o := options{userAgent: "delamain/dev", backoff: 2 * time.Second, timeout: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	hc := &http.Client{}
	if o.httpClient != nil {
		c := *o.httpClient
		hc = &c
	}
	if repo.Kind == GitLab {
		return newGitLab(repo, token, hc, o)
	}
	return newGitHub(repo, token, hc, o)
}

// CurrentUser returns the login token authenticates as on repo's forge (GET /user).
func CurrentUser(ctx context.Context, repo Repo, token string, opts ...Option) (string, error) {
	f, err := New(repo, token, opts...)
	if err != nil {
		return "", err
	}
	return f.(interface {
		user(context.Context) (string, error)
	}).user(ctx)
}

var scpLike = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// Detect parses the origin URL and picks the forge: kind "auto" recognizes github.com and hosts
// containing "gitlab"; "github"/"gitlab" force it for self-hosted instances.
func Detect(remoteURL, kind string) (Repo, error) {
	host, path, err := parseRemote(remoteURL)
	if err != nil {
		return Repo{}, err
	}
	repo := Repo{Host: host, Path: path}
	switch kind {
	case string(GitHub):
		repo.Kind = GitHub
	case string(GitLab):
		repo.Kind = GitLab
	case "", "auto":
		switch {
		case host == "github.com":
			repo.Kind = GitHub
		case strings.Contains(host, "gitlab"):
			repo.Kind = GitLab
		default:
			return Repo{}, fmt.Errorf("forge não reconhecido para o host %q; defina forge.kind (github|gitlab|none)", host)
		}
	default:
		return Repo{}, fmt.Errorf("forge.kind %q inválido", kind)
	}
	return repo, nil
}

func parseRemote(raw string) (host, path string, err error) {
	raw = strings.TrimSpace(raw)
	if m := scpLike.FindStringSubmatch(raw); m != nil {
		host, path = m[1], m[2]
	} else {
		u, perr := url.Parse(raw)
		if perr != nil || u.Host == "" {
			return "", "", fmt.Errorf("remote sem host de forge: %q", redact(raw))
		}
		host, path = u.Hostname(), u.Path // Hostname drops userinfo and port
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !strings.Contains(path, "/") {
		return "", "", fmt.Errorf("remote sem caminho owner/repo: %q", redact(raw))
	}
	return host, path, nil
}

// redact drops userinfo so tokens embedded in remote URLs never reach logs.
func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return raw
}

func oldestFirst(is []Issue) []Issue {
	slices.SortFunc(is, func(a, b Issue) int { return a.Number - b.Number })
	return is
}
