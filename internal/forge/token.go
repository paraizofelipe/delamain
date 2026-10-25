package forge

// The token is the one gh/glab would use: the same environment variables, in the same order,
// then the CLI's own stored login. This file is the only place delamain runs those CLIs, and
// only to read the token; every forge operation goes over HTTP.
//
// Minimum permissions:
//   - GitHub: a fine-grained PAT with Issues (read/write), Pull requests (read/write) and
//     Metadata (read) on the repositories delamain works on.
//   - GitLab: the Developer role and the api scope.
//
// A dedicated bot account keeps what delamain can touch separate from a person's access.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrTokenNotFound means no variable nor CLI login holds a token for the host.
var ErrTokenNotFound = errors.New("forge token not found")

// TokenLookup is where Token looks: the environment and the forge CLIs.
type TokenLookup interface {
	Getenv(key string) string
	Exec(ctx context.Context, name string, args ...string) (string, error)
}

// cliTimeout bounds the CLI fallback so a hung keyring never stalls delamain.
const cliTimeout = 10 * time.Second

var (
	cacheMu sync.Mutex
	cache   = map[string]string{} // kind+host → token read from the CLI
)

func resetTokenCache() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	clear(cache)
}

// Token resolves the token for host. source says where it came from (e.g. "env GH_TOKEN")
// and never carries the value itself.
func Token(ctx context.Context, kind Kind, host string, lookup TokenLookup) (token, source string, err error) {
	for _, key := range tokenVars(kind, host) {
		if v := strings.TrimSpace(lookup.Getenv(key)); v != "" {
			return v, "env " + key, nil
		}
	}
	name, args := "gh", []string{"auth", "token", "--hostname", host}
	source = "gh auth token"
	if kind == GitLab {
		name, args = "glab", []string{"config", "get", "token", "--host", host}
		source = "glab config get token"
	}

	key := string(kind) + "|" + host
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if v, ok := cache[key]; ok {
		return v, source, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	out, err := lookup.Exec(ctx, name, args...)
	if v := strings.TrimSpace(out); err == nil && v != "" {
		cache[key] = v
		return v, source, nil
	}
	// The exec error says nothing useful (not installed, not logged in) and may echo output.
	return "", "", fmt.Errorf("no %s token for %s: %w", kind, host, ErrTokenNotFound)
}

// OAuth reports whether the GitLab token Token found under source is an OAuth token, which goes
// as a bearer token (WithOAuth): OAUTH_TOKEN always is, and so is a glab login made through the
// browser, which glab marks with is_oauth2. renew keeps a glab login's token fresh; it is nil for
// OAUTH_TOKEN, which nothing here can renew.
func OAuth(ctx context.Context, kind Kind, host, source string, lookup TokenLookup) (renew TokenSource, ok bool) {
	if kind != GitLab {
		return nil, false
	}
	switch source {
	case "env OAUTH_TOKEN":
		return nil, true
	case "glab config get token":
		ctx, cancel := context.WithTimeout(ctx, cliTimeout)
		defer cancel()
		out, err := lookup.Exec(ctx, "glab", "config", "get", "is_oauth2", "--host", host)
		if err != nil || strings.TrimSpace(out) != "true" {
			return nil, false
		}
		return &glabOAuth{host: host, lookup: lookup, now: time.Now}, true
	}
	return nil, false
}

const (
	// renewAhead renews a glab OAuth token this long before it expires, so no request goes out
	// with a token about to lapse.
	renewAhead = time.Minute
	// renewRetry spaces the attempts while glab still holds a token past renewAhead: glab may
	// renew only once the token has actually expired.
	renewRetry = 30 * time.Second
)

// glabOAuth is the token of a glab OAuth login, which expires (two hours on gitlab.com). glab
// renews it on its own API calls and stores the new one, so once it is about to expire one cheap
// call through glab renews it and the token is read back. Token's cache never sees it.
type glabOAuth struct {
	host   string
	lookup TokenLookup
	now    func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time // zero when glab stores none: the token is then kept as read
	retry  time.Time // no renewal attempt before this
}

func (g *glabOAuth) Token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	expiring := func(expiry time.Time) bool { return !expiry.IsZero() && !now.Before(expiry.Add(-renewAhead)) }
	if g.token != "" && (!expiring(g.expiry) || now.Before(g.retry)) {
		return g.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	token, expiry, err := g.read(ctx)
	if err == nil && expiring(expiry) {
		// The output and the error are of no use: what matters is the token glab stores.
		_, _ = g.lookup.Exec(ctx, "glab", "api", "--hostname", g.host, "user")
		token, expiry, err = g.read(ctx)
	}
	g.retry = now.Add(renewRetry)
	if err != nil {
		if g.token != "" {
			return g.token, nil
		}
		return "", err
	}
	g.token, g.expiry = token, expiry
	return token, nil
}

// read reads the token glab stores for the host and when it expires.
func (g *glabOAuth) read(ctx context.Context) (string, time.Time, error) {
	out, err := g.lookup.Exec(ctx, "glab", "config", "get", "token", "--host", g.host)
	token := strings.TrimSpace(out)
	if err != nil || token == "" {
		return "", time.Time{}, fmt.Errorf("no %s token for %s: %w", GitLab, g.host, ErrTokenNotFound)
	}
	var expiry time.Time
	if out, err := g.lookup.Exec(ctx, "glab", "config", "get", "oauth2_expiry_date", "--host", g.host); err == nil {
		expiry, _ = time.Parse(time.RFC3339, strings.TrimSpace(out))
	}
	return token, expiry, nil
}

// TokenHint tells how to provide a token for host: the variables Token reads and the CLI login.
func TokenHint(kind Kind, host string) string {
	cli := "gh auth login --hostname " + host
	if kind == GitLab {
		cli = "glab auth login --hostname " + host
	}
	return "set " + strings.Join(tokenVars(kind, host), " or ") + ", or run `" + cli + "`"
}

// TokenPermissions is the least a token needs on kind's forge for delamain to work.
func TokenPermissions(kind Kind) string {
	if kind == GitLab {
		return "the token needs the Developer role and the api scope; a dedicated bot account is recommended"
	}
	return "the token needs Issues and Pull requests read/write and Metadata read (fine-grained PAT); a dedicated bot account is recommended"
}

// tokenVars lists the variables gh/glab read for host, in their order of precedence.
func tokenVars(kind Kind, host string) []string {
	switch {
	case kind == GitLab:
		return []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN"}
	case host == "github.com":
		return []string{"GH_TOKEN", "GITHUB_TOKEN"}
	default:
		return []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	}
}

// OSLookup reads the real environment and runs the real CLIs, without stdin and with
// prompts disabled so a missing login fails instead of waiting for input.
type OSLookup struct{}

func (OSLookup) Getenv(key string) string { return os.Getenv(key) }

func (OSLookup) Exec(ctx context.Context, name string, args ...string) (string, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &out
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_PROMPT=1")
	err := cmd.Run()
	return out.String(), err
}
