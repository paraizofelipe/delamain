package forge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLookup answers Getenv from env and Exec from out, keyed by the full command line.
type fakeLookup struct {
	env map[string]string
	out map[string]string
	err error

	mu    sync.Mutex
	execs []string
}

func (f *fakeLookup) Getenv(key string) string { return f.env[key] }

func (f *fakeLookup) Exec(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.execs = append(f.execs, line)
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.out[line], nil
}

func TestTokenShouldPreferGHTokenOverGitHubToken(t *testing.T) {
	l := &fakeLookup{env: map[string]string{"GH_TOKEN": "gh-value", "GITHUB_TOKEN": "github-value"}}
	tok, src, err := Token(context.Background(), GitHub, "github.com", l)
	if err != nil || tok != "gh-value" || src != "GH_TOKEN" {
		t.Fatalf("token=%q source=%q err=%v; want gh-value from GH_TOKEN", tok, src, err)
	}
	l.env = map[string]string{"GITHUB_TOKEN": "github-value"}
	tok, src, err = Token(context.Background(), GitHub, "github.com", l)
	if err != nil || tok != "github-value" || src != "GITHUB_TOKEN" {
		t.Fatalf("token=%q source=%q err=%v; want github-value from GITHUB_TOKEN", tok, src, err)
	}
	if len(l.execs) != 0 {
		t.Fatalf("a token in the env must not run the CLI: %v", l.execs)
	}
}

func TestTokenShouldUseEnterpriseVariablesForNonGitHubComHosts(t *testing.T) {
	l := &fakeLookup{env: map[string]string{"GH_TOKEN": "dotcom", "GITHUB_TOKEN": "dotcom", "GH_ENTERPRISE_TOKEN": "ghe-1", "GITHUB_ENTERPRISE_TOKEN": "ghe-2"}}
	tok, src, err := Token(context.Background(), GitHub, "ghe.corp.example", l)
	if err != nil || tok != "ghe-1" || src != "GH_ENTERPRISE_TOKEN" {
		t.Fatalf("token=%q source=%q err=%v; want ghe-1 from GH_ENTERPRISE_TOKEN", tok, src, err)
	}
	delete(l.env, "GH_ENTERPRISE_TOKEN")
	tok, src, err = Token(context.Background(), GitHub, "ghe.corp.example", l)
	if err != nil || tok != "ghe-2" || src != "GITHUB_ENTERPRISE_TOKEN" {
		t.Fatalf("token=%q source=%q err=%v; want ghe-2 from GITHUB_ENTERPRISE_TOKEN", tok, src, err)
	}
	tok, _, err = Token(context.Background(), GitHub, "GitHub.com", l)
	if err != nil || tok != "dotcom" {
		t.Fatalf("github.com is matched case-insensitively: token=%q err=%v", tok, err)
	}
}

func TestTokenShouldReadGitLabVariablesInOrder(t *testing.T) {
	l := &fakeLookup{env: map[string]string{"GITLAB_TOKEN": "a", "GITLAB_ACCESS_TOKEN": "b", "OAUTH_TOKEN": "c", "GH_TOKEN": "gh"}}
	for _, want := range []struct{ token, source string }{{"a", "GITLAB_TOKEN"}, {"b", "GITLAB_ACCESS_TOKEN"}, {"c", "OAUTH_TOKEN"}} {
		tok, src, err := Token(context.Background(), GitLab, "gitlab.corp.example", l)
		if err != nil || tok != want.token || src != want.source {
			t.Fatalf("token=%q source=%q err=%v; want %q from %s", tok, src, err, want.token, want.source)
		}
		delete(l.env, want.source)
	}
}

func TestTokenShouldFallBackToGhAuthTokenWithHostname(t *testing.T) {
	l := &fakeLookup{out: map[string]string{"gh auth token --hostname ghe.corp.example": "from-cli\n"}}
	tok, src, err := Token(context.Background(), GitHub, "ghe.corp.example", l)
	if err != nil || tok != "from-cli" || src != "gh auth token" {
		t.Fatalf("token=%q source=%q err=%v execs=%v", tok, src, err, l.execs)
	}
}

func TestTokenShouldFallBackToGlabConfigGetTokenWithHost(t *testing.T) {
	l := &fakeLookup{out: map[string]string{"glab config get token --host gitlab.com": "glpat-cli"}}
	tok, src, err := Token(context.Background(), GitLab, "gitlab.com", l)
	if err != nil || tok != "glpat-cli" || src != "glab config get token" {
		t.Fatalf("token=%q source=%q err=%v execs=%v", tok, src, err, l.execs)
	}
}

func TestTokenShouldTreatBlankValuesAsMissing(t *testing.T) {
	l := &fakeLookup{
		env: map[string]string{"GH_TOKEN": "  ", "GITHUB_TOKEN": "real"},
		out: map[string]string{"glab config get token --host gitlab.com": " \n"},
	}
	if tok, src, err := Token(context.Background(), GitHub, "github.com", l); err != nil || tok != "real" || src != "GITHUB_TOKEN" {
		t.Fatalf("a blank GH_TOKEN is skipped: token=%q source=%q err=%v", tok, src, err)
	}
	if _, _, err := Token(context.Background(), GitLab, "gitlab.com", l); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("blank CLI output is no token, err=%v", err)
	}
}

func TestTokenShouldReturnNotFoundWhenCLIIsMissing(t *testing.T) {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("PATH", t.TempDir())
	for _, c := range []struct {
		kind Kind
		host string
	}{{GitHub, "github.com"}, {GitLab, "gitlab.com"}} {
		tok, _, err := Token(context.Background(), c.kind, c.host, OSLookup{})
		if !errors.Is(err, ErrTokenNotFound) || tok != "" {
			t.Fatalf("%s without CLI: token=%q err=%v; want ErrTokenNotFound", c.kind, tok, err)
		}
	}
}

func TestTokenShouldCacheTheCLIFallbackPerHost(t *testing.T) {
	fake := &fakeLookup{out: map[string]string{
		"gh auth token --hostname github.com":       "t1",
		"gh auth token --hostname ghe.corp.example": "t2",
	}}
	l := CachedLookup(fake)
	for range 3 {
		if tok, _, err := Token(context.Background(), GitHub, "github.com", l); err != nil || tok != "t1" {
			t.Fatalf("token=%q err=%v", tok, err)
		}
	}
	if tok, _, err := Token(context.Background(), GitHub, "ghe.corp.example", l); err != nil || tok != "t2" {
		t.Fatalf("token=%q err=%v", tok, err)
	}
	if len(fake.execs) != 2 {
		t.Fatalf("the CLI runs once per host, got %v", fake.execs)
	}

	missing := &fakeLookup{err: errors.New("exit status 1")}
	l = CachedLookup(missing)
	for range 2 {
		if _, _, err := Token(context.Background(), GitLab, "gitlab.com", l); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("err=%v", err)
		}
	}
	if len(missing.execs) != 1 {
		t.Fatalf("a failed lookup is cached too, got %v", missing.execs)
	}

	fake.env = map[string]string{"GH_TOKEN": "fresh"}
	if tok, _, _ := Token(context.Background(), GitHub, "github.com", CachedLookup(fake)); tok != "fresh" {
		t.Fatalf("env is read on every call, got %q", tok)
	}
}

func TestTokenSourceShouldNeverContainTheTokenValue(t *testing.T) {
	const secret = "ghp_S3CR3TVALUE"
	cases := []TokenLookup{
		&fakeLookup{env: map[string]string{"GH_TOKEN": secret}},
		&fakeLookup{env: map[string]string{"GITHUB_TOKEN": secret}},
		&fakeLookup{out: map[string]string{"gh auth token --hostname github.com": secret}},
	}
	for _, l := range cases {
		tok, src, err := Token(context.Background(), GitHub, "github.com", l)
		if err != nil || tok != secret {
			t.Fatalf("token=%q err=%v", tok, err)
		}
		if src == "" || strings.Contains(src, secret) {
			t.Fatalf("source %q must name the origin only", src)
		}
	}
	_, _, err := Token(context.Background(), GitHub, "github.com", &fakeLookup{err: errors.New("boom " + secret)})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("not-found error must not echo CLI output: %v", err)
	}
}

// fakeCLI puts an executable named name, running script, first in PATH.
func fakeCLI(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestOSLookupShouldRunTheCLIWithoutStdinAndWithPromptsDisabled(t *testing.T) {
	fakeCLI(t, "gh", `in=$(cat); printf '%s|%s|%s|[%s]' "$*" "$GH_PROMPT_DISABLED" "$NO_PROMPT" "$in"`)
	out, err := OSLookup{}.Exec(context.Background(), "gh", "auth", "token", "--hostname", "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if out != "auth token --hostname github.com|1|1|[]" {
		t.Fatalf("out = %q", out)
	}
}

func TestOSLookupShouldKillTheCLIAfterItsTimeout(t *testing.T) {
	fakeCLI(t, "glab", `sleep 30`)
	start := time.Now()
	_, err := OSLookup{Timeout: 100 * time.Millisecond}.Exec(context.Background(), "glab", "config", "get", "token")
	if err == nil {
		t.Fatal("a hung CLI must fail")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the CLI must be killed after its timeout, took %v", d)
	}
}
