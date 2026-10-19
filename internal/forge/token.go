package forge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrTokenNotFound means neither the environment nor the forge CLI holds a token for the host.
var ErrTokenNotFound = errors.New("forge token not found")

// TokenLookup is where Token reads credentials from: the environment and the forge CLI.
type TokenLookup interface {
	Getenv(key string) string
	// Exec runs a command and returns its trimmed stdout.
	Exec(ctx context.Context, name string, args ...string) (string, error)
}

// TokenVars are the environment variables gh/glab read for host, in their precedence order.
func TokenVars(kind Kind, host string) []string {
	switch {
	case kind == GitLab:
		return []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN"}
	case strings.EqualFold(host, "github.com"):
		return []string{"GH_TOKEN", "GITHUB_TOKEN"}
	default:
		return []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
	}
}

// TokenHint tells how to give delamain a token for host: its variables or the CLI's login.
func TokenHint(kind Kind, host string) string {
	login := "gh auth login"
	if kind == GitLab {
		login = "glab auth login"
	}
	if !strings.EqualFold(host, "github.com") && !strings.EqualFold(host, "gitlab.com") {
		login += " --hostname " + host
	}
	return fmt.Sprintf("set %s, or run %s", strings.Join(TokenVars(kind, host), " or "), login)
}

// TokenPermissions is the least a token needs for what delamain does: read issues, comment,
// label and open PRs/MRs. It never needs to merge, push or administer the repository.
func TokenPermissions(kind Kind) string {
	if kind == GitLab {
		return "needs the api scope and at least the Developer role"
	}
	return "needs Issues and Pull requests read/write plus Metadata read (fine-grained token), or the repo scope (classic token)"
}

// Token resolves the token for host as gh/glab do: the host's environment variables first, then
// what the CLI itself stored on login. source names where the token came from, never its value.
func Token(ctx context.Context, kind Kind, host string, lookup TokenLookup) (token, source string, err error) {
	for _, k := range TokenVars(kind, host) {
		if v := strings.TrimSpace(lookup.Getenv(k)); v != "" {
			return v, k, nil
		}
	}
	name, args := "gh", []string{"auth", "token", "--hostname", host}
	source = "gh auth token"
	if kind == GitLab {
		name, args = "glab", []string{"config", "get", "token", "--host", host}
		source = "glab config get token"
	}
	// The CLI's error and output are dropped: either may carry credentials.
	out, err := lookup.Exec(ctx, name, args...)
	if v := strings.TrimSpace(out); err == nil && v != "" {
		return v, source, nil
	}
	return "", "", fmt.Errorf("%w for %s", ErrTokenNotFound, host)
}

// OSLookup reads the process environment and runs the real forge CLI, non-interactively.
type OSLookup struct {
	// Timeout bounds each CLI run; zero means 10s.
	Timeout time.Duration
}

func (OSLookup) Getenv(key string) string { return os.Getenv(key) }

func (l OSLookup) Exec(ctx context.Context, name string, args ...string) (string, error) {
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil // /dev/null: a CLI asking for input fails instead of hanging
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_PROMPT=1")
	cmd.WaitDelay = time.Second // do not wait on children still holding stdout after the kill
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// CachedLookup runs each distinct command once per process, remembering failures too, so the
// CLI fallback costs one run per host. The environment is read on every call.
func CachedLookup(l TokenLookup) TokenLookup {
	return &cachedLookup{TokenLookup: l, results: map[string]cached{}}
}

type cached struct {
	out string
	err error
}

type cachedLookup struct {
	TokenLookup
	mu      sync.Mutex
	results map[string]cached
}

func (c *cachedLookup) Exec(ctx context.Context, name string, args ...string) (string, error) {
	key := strings.Join(append([]string{name}, args...), "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.results[key]; ok {
		return r.out, r.err
	}
	out, err := c.TokenLookup.Exec(ctx, name, args...)
	c.results[key] = cached{out, err}
	return out, err
}
