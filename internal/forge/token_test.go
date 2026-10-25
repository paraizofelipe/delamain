package forge

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// forgeCLI matches a Go string literal that names the gh or glab CLI.
var forgeCLI = regexp.MustCompile(`"(gh|glab)(\s[^"]*)?"`)

func TestOnlyTokenGoShouldNameTheForgeCLIs(t *testing.T) {
	root := filepath.Join("..") // internal/
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if filepath.ToSlash(path) == "../forge/token.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if m := forgeCLI.Find(b); m != nil {
			t.Errorf("%s names a forge CLI (%s): only token.go may, to read the token", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOAuthShouldFollowWhereTheGitLabTokenCameFrom(t *testing.T) {
	ctx := context.Background()
	glab := &fakeLookup{out: map[string]string{"glab config get is_oauth2 --host gitlab.com": "true\n"}}
	cases := []struct {
		name   string
		kind   Kind
		source string
		lookup *fakeLookup
		want   bool
	}{
		{"glab logged in with OAuth", GitLab, "glab config get token", glab, true},
		{"glab logged in with a PAT", GitLab, "glab config get token", &fakeLookup{out: map[string]string{"glab config get is_oauth2 --host gitlab.com": "\n"}}, false},
		{"glab without the key", GitLab, "glab config get token", &fakeLookup{}, false},
		{"OAUTH_TOKEN", GitLab, "env OAUTH_TOKEN", &fakeLookup{}, true},
		{"GITLAB_TOKEN is a PAT", GitLab, "env GITLAB_TOKEN", glab, false},
		{"GitHub", GitHub, "gh auth token", glab, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := OAuth(ctx, c.kind, "gitlab.com", c.source, c.lookup); got != c.want {
				t.Fatalf("OAuth = %v, want %v", got, c.want)
			}
		})
	}
	if len(glab.calls) != 1 {
		t.Fatalf("only a glab login asks glab: %v", glab.calls)
	}
}

func TestOAuthShouldRenewOnlyAGlabLogin(t *testing.T) {
	glab := &fakeLookup{out: map[string]string{"glab config get is_oauth2 --host gitlab.com": "true\n"}}
	if renew, _ := OAuth(context.Background(), GitLab, "gitlab.com", "glab config get token", glab); renew == nil {
		t.Fatal("a glab OAuth login must come with a renewing source")
	}
	if renew, ok := OAuth(context.Background(), GitLab, "gitlab.com", "env OAUTH_TOKEN", glab); !ok || renew != nil {
		t.Fatalf("OAUTH_TOKEN goes as it is: %v %v", renew, ok)
	}
}

// glabStore plays glab: `glab api` renews the token it stores once that one has expired.
type glabStore struct {
	fakeLookup
	now      *time.Time
	token    string
	expiry   time.Time
	renewals int
}

func (g *glabStore) Exec(ctx context.Context, name string, args ...string) (string, error) {
	_, _ = g.fakeLookup.Exec(ctx, name, args...)
	switch strings.Join(args, " ") {
	case "config get token --host gitlab.com":
		return g.token + "\n", nil
	case "config get oauth2_expiry_date --host gitlab.com":
		return g.expiry.Format(time.RFC3339) + "\n", nil
	case "api --hostname gitlab.com user":
		if !g.now.Before(g.expiry) {
			g.renewals++
			g.token, g.expiry = fmt.Sprintf("oauth-%d", g.renewals+1), g.now.Add(2*time.Hour)
		}
		return "{}", nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func TestGlabOAuthShouldRenewTheTokenAsItExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	store := &glabStore{now: &now, token: "oauth-1", expiry: now.Add(2 * time.Hour)}
	src := &glabOAuth{host: "gitlab.com", lookup: store, now: func() time.Time { return now }}
	token := func() string {
		t.Helper()
		tok, err := src.Token(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	if got := token(); got != "oauth-1" {
		t.Fatalf("first token = %q", got)
	}
	now = now.Add(time.Hour)
	before := len(store.calls)
	if got := token(); got != "oauth-1" || len(store.calls) != before {
		t.Fatalf("a token far from expiring must be kept without asking glab: %q %v", got, store.calls[before:])
	}

	// Within renewAhead glab may not renew yet: the token is kept and glab is not asked again at once.
	now = store.expiry.Add(-renewAhead / 2)
	if got := token(); got != "oauth-1" {
		t.Fatalf("token just before the expiry = %q", got)
	}
	before = len(store.calls)
	if got := token(); got != "oauth-1" || len(store.calls) != before {
		t.Fatalf("glab is asked again before renewRetry: %v", store.calls[before:])
	}

	now = store.expiry.Add(time.Second)
	if got := token(); got != "oauth-2" || store.renewals != 1 {
		t.Fatalf("token after the expiry = %q (renewals %d)", got, store.renewals)
	}
}

func TestGlabOAuthShouldRenewAnExpiredTokenOnTheFirstRead(t *testing.T) {
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	store := &glabStore{now: &now, token: "oauth-1", expiry: now.Add(-time.Hour)}
	src := &glabOAuth{host: "gitlab.com", lookup: store, now: func() time.Time { return now }}
	if got, err := src.Token(context.Background()); err != nil || got != "oauth-2" {
		t.Fatalf("token = %q, err = %v", got, err)
	}
}

func TestGlabOAuthShouldFailWithoutALogin(t *testing.T) {
	src := &glabOAuth{host: "gitlab.com", lookup: &fakeLookup{}, now: time.Now}
	if _, err := src.Token(context.Background()); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("err = %v", err)
	}
}
