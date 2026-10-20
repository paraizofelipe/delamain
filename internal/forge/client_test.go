package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// forgeCase runs a test against both forges through the same public method, Comment.
type forgeCase struct {
	name    string
	at      func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
	comment string // the comment endpoint the fake serves
}

var bothForges = []forgeCase{
	{"github", githubAt, "/repos/acme/api/issues/7/comments"},
	{"gitlab", gitlabAt, glProject + "/issues/7/notes"},
}

// sequence answers the nth request with steps[n], repeating the last step after the end.
func sequence(steps ...func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		i := int(n.Add(1)) - 1
		steps[min(i, len(steps)-1)](w, r)
	}
}

// hangUp drops the connection without answering: a network failure.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		c.Close()
	}
}

// newServer serves h for the test and returns its URL.
func newServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func countOf(api *fakeAPI, method, path string) int {
	n := 0
	for _, r := range api.requests() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func TestClientShouldRetryTransientErrorsThreeTimes(t *testing.T) {
	failures := map[string]func(http.ResponseWriter, *http.Request){
		"502": reply(502, `{"message":"Bad Gateway"}`), "503": reply(503, `{"message":"Service Unavailable"}`),
		"504": reply(504, `{"message":"Gateway Timeout"}`), "network": hangUp,
	}
	for _, fc := range bothForges {
		for name, fail := range failures {
			t.Run(fc.name+"/"+name, func(t *testing.T) {
				f, api := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){
					"POST " + fc.comment: sequence(fail, fail, reply(201, `{"id":1}`)),
				})
				if err := f.Comment(context.Background(), 7, "c"); err != nil {
					t.Fatalf("the third attempt succeeds: %v", err)
				}
				if n := countOf(api, "POST", fc.comment); n != 3 {
					t.Fatalf("attempts = %d, want 3", n)
				}

				f, api = fc.at(t, map[string]func(http.ResponseWriter, *http.Request){"POST " + fc.comment: fail})
				if err := f.Comment(context.Background(), 7, "c"); err == nil {
					t.Fatal("a failure that never clears must be reported")
				}
				if n := countOf(api, "POST", fc.comment); n != 3 {
					t.Fatalf("attempts = %d, want to give up after 3", n)
				}
			})
		}
	}
}

func TestClientShouldNotRetryClientErrors(t *testing.T) {
	for _, fc := range bothForges {
		for _, status := range []int{400, 401, 403, 404, 422, 429, 500} {
			f, api := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST " + fc.comment: reply(status, `{"message":"rejected"}`),
			})
			err := f.Comment(context.Background(), 7, "c")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != status {
				t.Errorf("%s %d: want APIError with that status, got %v", fc.name, status, err)
			}
			if n := countOf(api, "POST", fc.comment); n != 1 {
				t.Errorf("%s %d: attempts = %d, an API rejection is not retried", fc.name, status, n)
			}
		}
	}
}

func TestGitHubClientShouldHonorRetryAfterOnSecondaryRateLimit(t *testing.T) {
	limited := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
	}
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/issues/7/comments": sequence(limited, reply(201, `{"id":1}`)),
	})
	start := time.Now()
	if err := f.Comment(context.Background(), 7, "c"); err != nil {
		t.Fatalf("a secondary rate limit clears after Retry-After: %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("retried after %v, before the 1s Retry-After", waited)
	}
	if n := countOf(api, "POST", "/repos/acme/api/issues/7/comments"); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

func TestClientShouldStopRetryingWhenContextIsCancelled(t *testing.T) {
	for _, fc := range bothForges {
		ctx, cancel := context.WithCancel(context.Background())
		f, api := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST " + fc.comment: func(w http.ResponseWriter, r *http.Request) {
				reply(503, `{"message":"Service Unavailable"}`)(w, r)
				time.AfterFunc(100*time.Millisecond, cancel) // while the client waits to retry
			},
		}, WithBackoff(time.Hour))
		done := make(chan error, 1)
		go func() { done <- f.Comment(ctx, 7, "c") }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: a cancelled call must fail", fc.name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: shutdown must not wait out the backoff", fc.name)
		}
		if n := countOf(api, "POST", fc.comment); n != 1 {
			t.Errorf("%s: attempts = %d after cancel, want 1", fc.name, n)
		}
		cancel()
	}
}

func TestClientShouldResendTheSameBodyOnRetry(t *testing.T) {
	for _, fc := range bothForges {
		f, api := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST " + fc.comment: sequence(reply(502, `{"message":"Bad Gateway"}`), reply(201, `{"id":1}`)),
		})
		if err := f.Comment(context.Background(), 7, "the full comment"); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, r := range api.requests() {
			bodies = append(bodies, r.body)
		}
		if len(bodies) != 2 || bodies[0] != bodies[1] || !strings.Contains(bodies[1], "the full comment") {
			t.Fatalf("%s: retried bodies = %q", fc.name, bodies)
		}
	}
}

func TestErrorsShouldNeverContainTheToken(t *testing.T) {
	const token = "s3cr3t-T0KEN-value"
	for _, kind := range []Kind{GitHub, GitLab} {
		srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			reply(401, `{"message":"Bad credentials for `+token+` at https://user:`+token+`@example.com/x"}`)(w, r)
		})
		f, err := New(Repo{kind, "example.com", "acme/api"}, token, WithBaseURL(srv+"/"), WithBackoff(1))
		if err != nil {
			t.Fatal(err)
		}
		err = f.Comment(context.Background(), 7, "c")
		if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "401") {
			t.Fatalf("%s: error = %v", kind, err)
		}
	}
}

func TestErrorsShouldBeTruncatedAt500Characters(t *testing.T) {
	for _, fc := range bothForges {
		f, _ := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST " + fc.comment: reply(422, `{"message":"`+strings.Repeat("é", 2000)+`"}`),
		})
		err := f.Comment(context.Background(), 7, "c")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: %v", fc.name, err)
		}
		if n := utf8.RuneCountInString(apiErr.Message); n > 501 || !utf8.ValidString(err.Error()) {
			t.Fatalf("%s: message has %d characters, want at most 500 plus an ellipsis", fc.name, n)
		}
	}
}

// The whole point of delamain's delivery: it opens PRs/MRs and never merges them.
func TestForgeShouldNeverCallMergeEndpoints(t *testing.T) {
	for _, repo := range []Repo{{GitHub, "github.com", "acme/api"}, {GitLab, "gitlab.com", "grp/api"}} {
		var (
			mu    sync.Mutex
			paths []string
		)
		srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.Method+" "+r.URL.EscapedPath())
			mu.Unlock()
			body := `{"number":1,"iid":1}`
			if r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/1") {
				body = `[]`
			}
			reply(200, body)(w, r)
		})
		f, err := New(repo, "tok", WithBaseURL(srv+"/"), WithBackoff(1))
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		_, _ = f.Issue(ctx, 1)
		_, _ = f.ListReady(ctx, "ready")
		_, _ = f.OpenPR(ctx, "feat/x")
		_, _ = f.CreatePR(ctx, PROptions{Head: "feat/x", Base: "main", Title: "t"})
		_ = f.Comment(ctx, 1, "c")
		_ = f.Relabel(ctx, 1, []string{"a"}, []string{"b"})
		if len(paths) < 6 {
			t.Fatalf("%s: only %d requests reached the fake: %v", repo.Kind, len(paths), paths)
		}
		for _, p := range paths {
			for _, seg := range strings.Split(p, "/") {
				if seg == "merge" {
					t.Errorf("%s: forbidden merge call %q", repo.Kind, p)
				}
			}
		}
	}
}
