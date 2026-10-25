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
// browser, which glab marks with is_oauth2.
func OAuth(ctx context.Context, kind Kind, host, source string, lookup TokenLookup) bool {
	if kind != GitLab {
		return false
	}
	switch source {
	case "env OAUTH_TOKEN":
		return true
	case "glab config get token":
		ctx, cancel := context.WithTimeout(ctx, cliTimeout)
		defer cancel()
		out, err := lookup.Exec(ctx, "glab", "config", "get", "is_oauth2", "--host", host)
		return err == nil && strings.TrimSpace(out) == "true"
	}
	return false
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
