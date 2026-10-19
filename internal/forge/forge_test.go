package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// serve starts a plain HTTP API stub; clients reach it through WithBaseURL.
func serve(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// serveTLS starts an HTTPS API stub on 127.0.0.1:<port>, so a Repo whose Host is that address
// exercises the base URL the client derives from the host.
func serveTLS(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// request is what an API stub received.
type request struct {
	Method, Path, ContentType string
	Query                     url.Values
	Body                      map[string]any
	List                      []any // body, when it is a JSON array
}

// recorder keeps every request an API stub receives.
type recorder struct {
	mu   sync.Mutex
	reqs []request
}

func (rc *recorder) add(r *http.Request) request {
	b, _ := io.ReadAll(r.Body)
	req := request{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Query: r.URL.Query()}
	if json.Unmarshal(b, &req.Body) != nil {
		_ = json.Unmarshal(b, &req.List)
	}
	rc.mu.Lock()
	rc.reqs = append(rc.reqs, req)
	rc.mu.Unlock()
	return req
}

// find returns the requests with method on path.
func (rc *recorder) find(method, path string) []request {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []request
	for _, r := range rc.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func writeStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestDetectShouldRecognizeForgeFromRemoteURL(t *testing.T) {
	cases := []struct {
		url, kind string
		want      Repo
	}{
		{"https://github.com/acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"git@github.com:acme/api.git", "auto", Repo{GitHub, "github.com", "acme/api"}},
		{"ssh://git@gitlab.com:2222/grp/sub/api.git", "auto", Repo{GitLab, "gitlab.com", "grp/sub/api"}},
		{"git@gitlab.luizalabs.com:team/api.git", "auto", Repo{GitLab, "gitlab.luizalabs.com", "team/api"}},
		{"https://git.corp.example/team/api", "gitlab", Repo{GitLab, "git.corp.example", "team/api"}},
		{"https://ghe.corp.example/team/api.git", "github", Repo{GitHub, "ghe.corp.example", "team/api"}},
	}
	for _, c := range cases {
		got, err := Detect(c.url, c.kind)
		if err != nil || got != c.want {
			t.Errorf("Detect(%q, %q) = %+v, %v; want %+v", c.url, c.kind, got, err, c.want)
		}
	}
}

func TestDetectShouldNeverKeepCredentialsFromTheURL(t *testing.T) {
	got, err := Detect("https://oauth2:glpat-SECRET@gitlab.com/grp/api.git", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.String(), "SECRET") || strings.Contains(got.String(), "oauth2") {
		t.Fatalf("credentials leaked into %q", got.String())
	}
}

func TestDetectShouldRejectUnknownHostsAndMalformedURLs(t *testing.T) {
	for _, u := range []string{"https://bitbucket.org/acme/api.git", "/srv/git/api.git", "", "https://github.com/", "git@github.com:"} {
		if _, err := Detect(u, "auto"); err == nil {
			t.Errorf("Detect(%q) must fail: unknown forge or no repository path", u)
		}
	}
}

// The whole point of delamain's delivery: it opens PRs/MRs and never merges them.
func TestForgeShouldNeverIssueMergeCommands(t *testing.T) {
	for _, kind := range []Kind{GitHub, GitLab} {
		rc := &recorder{}
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			req := rc.add(r)
			switch {
			case req.Method == http.MethodGet && (strings.HasSuffix(req.Path, "/issues") || strings.HasSuffix(req.Path, "/pulls") || strings.HasSuffix(req.Path, "/merge_requests")):
				writeJSON(w, `[]`)
			default:
				writeJSON(w, `{"number":1,"iid":1,"state":"open"}`)
			}
		})
		base := srv.URL + "/"
		f, err := New(Repo{kind, "example.com", "acme/api"}, "tok", WithBaseURL(base))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		_, _ = f.Issue(ctx, 1)
		_, _ = f.ListReady(ctx, "delamain:ready")
		_, _ = f.OpenPR(ctx, "feat/x")
		_, _ = f.CreatePR(ctx, PROptions{Head: "feat/x", Base: "main", Title: "t", Body: "b"})
		_ = f.Comment(ctx, 1, "c")
		_ = f.Relabel(ctx, 1, []string{"a"}, []string{"b"})
		if len(rc.reqs) < 6 {
			t.Fatalf("%s: every method must reach the API, got %+v", kind, rc.reqs)
		}
		for _, r := range rc.reqs {
			if strings.HasSuffix(r.Path, "/merge") || strings.Contains(r.Path, "/merge/") {
				t.Errorf("%s: forbidden merge request %s %s", kind, r.Method, r.Path)
			}
			for _, k := range []string{"merge_when_pipeline_succeeds", "auto_merge", "merge_method"} {
				if hasKey(r.Body, k) {
					t.Errorf("%s: forbidden %s in %s %s", kind, k, r.Method, r.Path)
				}
			}
		}
	}
}
