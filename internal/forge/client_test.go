package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// posts counts the POSTs the fake received on path.
func posts(api *fakeAPI, path string) int {
	n := 0
	for _, r := range api.requests() {
		if r.method == http.MethodPost && r.path == path {
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
				if n := posts(api, fc.comment); n != 3 {
					t.Fatalf("attempts = %d, want 3", n)
				}

				f, api = fc.at(t, map[string]func(http.ResponseWriter, *http.Request){"POST " + fc.comment: fail})
				if err := f.Comment(context.Background(), 7, "c"); err == nil {
					t.Fatal("a failure that never clears must be reported")
				}
				if n := posts(api, fc.comment); n != 3 {
					t.Fatalf("attempts = %d, want to give up after 3", n)
				}
			})
		}
	}
}

func TestCreatePRShouldReturnThePRAnEarlierAttemptCreated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		create string
		exists func(http.ResponseWriter, *http.Request)
		open   string
		found  func(http.ResponseWriter, *http.Request)
		url    string
	}{
		{"github", githubAt, "/repos/acme/api/pulls", reply(422, `{"message":"A pull request already exists for acme:feat/x."}`),
			"/repos/acme/api/pulls", reply(200, `[{"number":42,"html_url":"https://github.com/acme/api/pull/42"}]`), "https://github.com/acme/api/pull/42"},
		{"gitlab", gitlabAt, glProject + "/merge_requests", reply(409, `{"message":["Another open merge request already exists for this source branch: !42"]}`),
			glProject + "/merge_requests", reply(200, `[{"iid":42,"web_url":"https://gitlab.com/grp/api/-/merge_requests/42"}]`), "https://gitlab.com/grp/api/-/merge_requests/42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, api := tc.at(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST " + tc.create: sequence(hangUp, tc.exists), // the first answer is lost, the PR was created
				"GET " + tc.open:    tc.found,
			})
			pr, err := f.CreatePR(context.Background(), PROptions{Title: "t", Head: "feat/x", Base: "main"})
			if err != nil || pr.URL != tc.url {
				t.Fatalf("the retry finds the PR the lost attempt created: %+v %v", pr, err)
			}
			if n := posts(api, tc.create); n != 2 {
				t.Fatalf("attempts = %d, want 2", n)
			}

			f, _ = tc.at(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST " + tc.create: tc.exists,
				"GET " + tc.open:    tc.found,
			})
			var apiErr *APIError
			if _, err := f.CreatePR(context.Background(), PROptions{Title: "t", Head: "feat/x", Base: "main"}); !errors.As(err, &apiErr) {
				t.Fatalf("on the first attempt, exists is somebody else's PR and stays an error: %v", err)
			}
		})
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
			if n := posts(api, fc.comment); n != 1 {
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
	if n := posts(api, "/repos/acme/api/issues/7/comments"); n != 2 {
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
		if n := posts(api, fc.comment); n != 1 {
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

func TestPRViewShouldFailWhenThePRDoesNotExist(t *testing.T) {
	for _, fc := range bothForges {
		f, _ := fc.at(t, map[string]func(http.ResponseWriter, *http.Request){})
		_, err := f.PRView(context.Background(), 404)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
			t.Errorf("%s: want an APIError with status 404, got %v", fc.name, err)
		}
	}
}

// readOnlyAPI answers every read a Forge makes with an empty but valid payload and records
// each request, so a test can check that none of them writes.
func readOnlyAPI(t *testing.T) (string, func() []request) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []request
	)
	base := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, request{method: r.Method, path: r.URL.EscapedPath(), body: string(b)})
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[]},"reviewThreads":{"nodes":[]}}}}}`)(w, r)
		case r.Method != http.MethodGet:
			reply(405, `{"message":"read only"}`)(w, r)
		case strings.HasSuffix(r.URL.Path, "/1"):
			reply(200, `{"number":1,"iid":1}`)(w, r)
		default:
			reply(200, `[]`)(w, r)
		}
	})
	return base, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), reqs...)
	}
}

// Review is read only: no new method may change anything on the forge.
func TestReadMethodsShouldNeverWriteToTheForge(t *testing.T) {
	for _, repo := range []Repo{{GitHub, "github.com", "acme/api"}, {GitLab, "gitlab.com", "grp/api"}} {
		t.Run(string(repo.Kind), func(t *testing.T) {
			srv, requests := readOnlyAPI(t)
			f, err := New(repo, "tok", WithBaseURL(srv+"/"), WithBackoff(1))
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			reads := map[string]func() error{
				"PRView":       func() error { _, err := f.PRView(ctx, 1); return err },
				"ListPRs":      func() error { _, err := f.ListPRs(ctx, "review"); return err },
				"LinkedIssues": func() error { _, err := f.LinkedIssues(ctx, 1); return err },
			}
			for name, read := range reads {
				before := len(requests())
				if err := read(); err != nil {
					t.Errorf("%s: %v", name, err)
				}
				if len(requests()) == before {
					t.Errorf("%s reached no endpoint of the fake", name)
				}
			}
			for _, r := range requests() {
				graphqlQuery := r.method == http.MethodPost && r.path == "/graphql" && !strings.HasPrefix(strings.TrimSpace(jsonQuery(t, r)), "mutation")
				if r.method != http.MethodGet && !graphqlQuery {
					t.Errorf("forbidden write %s %s %s", r.method, r.path, r.body)
				}
			}
		})
	}
}

// jsonQuery returns the GraphQL query a recorded request sent.
func jsonQuery(t *testing.T, r request) string {
	t.Helper()
	q, _ := jsonBody(t, r)["query"].(string)
	return q
}

// listPRsCase is a PR listing on one forge: the routes it serves and the query it must send.
type listPRsCase struct {
	name  string
	at    func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
	list  string // the listing endpoint
	state string // the forge's word for open
}

var listPRsCases = []listPRsCase{
	{"github", githubAt, "/repos/acme/api/issues", "open"},
	{"gitlab", gitlabAt, glProject + "/merge_requests", "opened"},
}

func TestListPRsShouldReturnOpenPRsWithLabelOldestFirst(t *testing.T) {
	routes := map[string]map[string]func(http.ResponseWriter, *http.Request){
		"github": {
			// the issues endpoint is the one that filters by label; it mixes issues and PRs
			"GET /repos/acme/api/issues": reply(200, `[
				{"number":9,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/9"}},
				{"number":4,"state":"open"},
				{"number":5,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/5"}}]`),
			"GET /repos/acme/api/pulls/9": reply(200, `{"number":9,"title":"nine","state":"open","head":{"ref":"feat/nine","sha":"h9"}}`),
			"GET /repos/acme/api/pulls/5": reply(200, `{"number":5,"title":"five","state":"open","head":{"ref":"feat/five","sha":"h5"}}`),
		},
		"gitlab": {
			"GET " + glProject + "/merge_requests": reply(200, `[
				{"iid":9,"title":"nine","state":"opened","source_branch":"feat/nine","sha":"h9"},
				{"iid":5,"title":"five","state":"opened","source_branch":"feat/five","sha":"h5"}]`),
		},
	}
	for _, tc := range listPRsCases {
		t.Run(tc.name, func(t *testing.T) {
			f, api := tc.at(t, routes[tc.name])
			prs, err := f.ListPRs(context.Background(), "delamain:review")
			if err != nil {
				t.Fatal(err)
			}
			if len(prs) != 2 || prs[0].Number != 5 || prs[1].Number != 9 {
				t.Fatalf("want PRs 5 then 9 and no plain issue, got %+v", prs)
			}
			if prs[0].Title != "five" || prs[0].HeadRef != "feat/five" || prs[0].HeadSHA != "h5" || !prs[0].Open {
				t.Fatalf("each PR carries its detail: %+v", prs[0])
			}
			q, _ := url.ParseQuery(api.find(t, "GET", tc.list).query)
			if q.Get("state") != tc.state || q.Get("labels") != "delamain:review" || q.Get("per_page") != "100" {
				t.Fatalf("must list open PRs by label, up to 100: %v", q)
			}
		})
	}
}

func TestListPRsShouldReturnEmptyWhenNoPRHasTheLabel(t *testing.T) {
	for _, tc := range listPRsCases {
		t.Run(tc.name, func(t *testing.T) {
			f, api := tc.at(t, map[string]func(http.ResponseWriter, *http.Request){"GET " + tc.list: reply(200, `[]`)})
			prs, err := f.ListPRs(context.Background(), "delamain:review")
			if err != nil || len(prs) != 0 {
				t.Fatalf("prs = %+v, err = %v", prs, err)
			}
			api.find(t, "GET", tc.list)
		})
	}
}

func TestLinkedIssuesShouldUseForgeReferences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":3},{"number":8}]}}}}}`),
			"GET /repos/acme/api/issues/3": reply(200, `{"number":3,"title":"three","state":"open"}`),
			"GET /repos/acme/api/issues/8": reply(200, `{"number":8,"title":"eight","state":"closed"}`),
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[{"iid":3},{"iid":8}]`),
			"GET " + glProject + "/issues/3":                        reply(200, `{"iid":3,"title":"three","state":"opened"}`),
			"GET " + glProject + "/issues/8":                        reply(200, `{"iid":8,"title":"eight","state":"closed"}`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, api := tc.at(t, tc.routes)
			is, err := f.LinkedIssues(context.Background(), 12)
			if err != nil {
				t.Fatal(err)
			}
			if len(is) != 2 || is[0].Number != 3 || is[0].Title != "three" || !is[0].Open || is[1].Number != 8 || is[1].Title != "eight" || is[1].Open {
				t.Fatalf("want issues 3 and 8 as the forge links them, got %+v", is)
			}
			if tc.name == "github" {
				var sent struct {
					Query     string         `json:"query"`
					Variables map[string]any `json:"variables"`
				}
				r := api.find(t, "POST", "/graphql")
				if err := json.Unmarshal([]byte(r.body), &sent); err != nil || !strings.Contains(sent.Query, "closingIssuesReferences(first:5)") ||
					sent.Variables["owner"] != "acme" || sent.Variables["name"] != "api" || sent.Variables["number"] != float64(12) {
					t.Fatalf("graphql request = %s (%v)", r.body, err)
				}
			}
		})
	}
}
