package forge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLookup is the environment and the forge CLIs as Token sees them.
type fakeLookup struct {
	env map[string]string
	out map[string]string // "name arg..." → stdout; missing means the CLI is not installed

	mu    sync.Mutex
	calls []string
	ctxs  []context.Context
}

func (f *fakeLookup) Getenv(k string) string { return f.env[k] }

func (f *fakeLookup) Exec(ctx context.Context, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	out, ok := f.out[cmd]
	if !ok {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return out, nil
}

func freshTokenCache(t *testing.T) {
	t.Helper()
	resetTokenCache()
	t.Cleanup(resetTokenCache)
}

func TestTokenShouldPreferGHTokenOverGitHubToken(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{env: map[string]string{"GH_TOKEN": "gh-tok", "GITHUB_TOKEN": "github-tok"}}
	tok, src, err := Token(context.Background(), GitHub, "github.com", l)
	if err != nil || tok != "gh-tok" || src != "env GH_TOKEN" {
		t.Fatalf("token = %q, source = %q, err = %v", tok, src, err)
	}
	l = &fakeLookup{env: map[string]string{"GITHUB_TOKEN": "github-tok"}}
	if tok, src, _ := Token(context.Background(), GitHub, "github.com", l); tok != "github-tok" || src != "env GITHUB_TOKEN" {
		t.Fatalf("GITHUB_TOKEN is the second choice: %q %q", tok, src)
	}
	if len(l.calls) != 0 {
		t.Fatalf("an env token must not run the CLI: %v", l.calls)
	}
}

func TestTokenShouldUseEnterpriseVariablesForNonGitHubComHosts(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{env: map[string]string{"GH_TOKEN": "public", "GITHUB_ENTERPRISE_TOKEN": "ghe-2"}}
	tok, src, err := Token(context.Background(), GitHub, "ghe.corp.example", l)
	if err != nil || tok != "ghe-2" || src != "env GITHUB_ENTERPRISE_TOKEN" {
		t.Fatalf("token = %q, source = %q, err = %v", tok, src, err)
	}
	l.env["GH_ENTERPRISE_TOKEN"] = "ghe-1"
	if tok, _, _ := Token(context.Background(), GitHub, "ghe.corp.example", l); tok != "ghe-1" {
		t.Fatalf("GH_ENTERPRISE_TOKEN comes first, got %q", tok)
	}
}

func TestTokenShouldReadGitLabVariablesInOrder(t *testing.T) {
	freshTokenCache(t)
	order := []string{"GITLAB_TOKEN", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN"}
	for i, key := range order {
		env := map[string]string{}
		for _, k := range order[i:] {
			env[k] = "tok-" + k
		}
		tok, src, err := Token(context.Background(), GitLab, "gitlab.example", &fakeLookup{env: env})
		if err != nil || tok != "tok-"+key || src != "env "+key {
			t.Fatalf("with %v: token = %q, source = %q, err = %v", order[i:], tok, src, err)
		}
	}
}

func TestTokenShouldFallBackToGhAuthTokenWithHostname(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{out: map[string]string{"gh auth token --hostname ghe.corp.example": "gho_cli\n"}}
	tok, src, err := Token(context.Background(), GitHub, "ghe.corp.example", l)
	if err != nil || tok != "gho_cli" || src != "gh auth token" {
		t.Fatalf("token = %q, source = %q, err = %v", tok, src, err)
	}
}

func TestTokenShouldFallBackToGlabConfigGetTokenWithHost(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{out: map[string]string{"glab config get token --host gitlab.example": "glpat-cli\n"}}
	tok, src, err := Token(context.Background(), GitLab, "gitlab.example", l)
	if err != nil || tok != "glpat-cli" || src != "glab config get token" {
		t.Fatalf("token = %q, source = %q, err = %v", tok, src, err)
	}
}

func TestTokenShouldTreatBlankValuesAsMissing(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{
		env: map[string]string{"GH_TOKEN": "  ", "GITHUB_TOKEN": "real"},
		out: map[string]string{"glab config get token --host gitlab.com": " \n"},
	}
	if tok, _, _ := Token(context.Background(), GitHub, "github.com", l); tok != "real" {
		t.Fatalf("a blank GH_TOKEN must be skipped, got %q", tok)
	}
	if _, _, err := Token(context.Background(), GitLab, "gitlab.com", l); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("blank CLI output is no token, got %v", err)
	}
}

func TestTokenShouldReturnNotFoundWhenCLIIsMissing(t *testing.T) {
	freshTokenCache(t)
	_, _, err := Token(context.Background(), GitHub, "github.com", &fakeLookup{})
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
	if strings.Contains(err.Error(), "executable file not found") {
		t.Fatalf("the exec failure must not leak into the error: %v", err)
	}
}

func TestTokenShouldCacheTheCLIFallbackPerHost(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{out: map[string]string{
		"gh auth token --hostname github.com":       "a",
		"gh auth token --hostname ghe.corp.example": "b",
	}}
	for range 3 {
		if tok, _, _ := Token(context.Background(), GitHub, "github.com", l); tok != "a" {
			t.Fatalf("token = %q", tok)
		}
	}
	if tok, _, _ := Token(context.Background(), GitHub, "ghe.corp.example", l); tok != "b" {
		t.Fatalf("each host has its own token, got %q", tok)
	}
	if len(l.calls) != 2 {
		t.Fatalf("the CLI must run once per host: %v", l.calls)
	}
}

func TestTokenShouldBoundTheCLIFallbackWithATimeout(t *testing.T) {
	freshTokenCache(t)
	l := &fakeLookup{out: map[string]string{"gh auth token --hostname github.com": "a"}}
	if _, _, err := Token(context.Background(), GitHub, "github.com", l); err != nil {
		t.Fatal(err)
	}
	dl, ok := l.ctxs[0].Deadline()
	if !ok || time.Until(dl) > 10*time.Second {
		t.Fatalf("the CLI fallback must be bounded by 10s: deadline=%v ok=%v", dl, ok)
	}
}

func TestTokenSourceShouldNeverContainTheTokenValue(t *testing.T) {
	freshTokenCache(t)
	cases := []*fakeLookup{
		{env: map[string]string{"GH_TOKEN": "ghp_SECRETVALUE"}},
		{out: map[string]string{"gh auth token --hostname github.com": "ghp_SECRETVALUE"}},
	}
	for _, l := range cases {
		tok, src, err := Token(context.Background(), GitHub, "github.com", l)
		if err != nil || tok != "ghp_SECRETVALUE" || strings.Contains(src, "SECRET") {
			t.Fatalf("source %q must name where the token came from, not carry it (err %v)", src, err)
		}
		resetTokenCache()
	}
}

func TestOSLookupShouldRunTheCLIWithoutStdinOrPrompts(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"stdin=$(cat)\n" +
		"fd0=$(readlink /proc/self/fd/0 2>/dev/null || echo none)\n" +
		`echo "args=$*|prompt=$GH_PROMPT_DISABLED|noprompt=$NO_PROMPT|stdin=$stdin|fd0=$fd0"` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := OSLookup{}.Exec(context.Background(), "gh", "auth", "token", "--hostname", "github.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "args=auth token --hostname github.com|prompt=1|noprompt=1|stdin=|") {
		t.Fatalf("out = %q", out)
	}
	if fd0 := out[strings.Index(out, "fd0=")+4:]; !strings.HasPrefix(fd0, "/dev/null") && !strings.HasPrefix(fd0, "none") {
		t.Fatalf("the CLI must not inherit stdin: %q", fd0)
	}
}
