package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
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
		_, _ = f.PRView(ctx, 1)
		_, _ = f.ListPRs(ctx, "review")
		_, _ = f.LinkedIssues(ctx, 1)
		_, _ = f.Threads(ctx, 1)
		_ = f.PostReview(ctx, 1, ReviewOptions{HeadSHA: "h", Summary: "s", Comments: []InlineComment{{Path: "a.go", Line: 1, Body: "c"}}})
		_ = f.PRComment(ctx, 1, "c")
		_ = f.Reply(ctx, 1, "RT_1", "c")
		_ = f.Reply(ctx, 1, "issue-comment:1", "c")
		_ = f.ResolveThread(ctx, 1, "RT_1")
		_ = f.RelabelPR(ctx, 1, []string{"a"}, []string{"b"})
		if len(paths) < 17 {
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
				"Threads":      func() error { _, err := f.Threads(ctx, 1); return err },
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
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":3,"repository":{"nameWithOwner":"acme/api"}},{"number":8,"repository":{"nameWithOwner":"Acme/API"}}]}}}}}`),
			"GET /repos/acme/api/issues/3": reply(200, `{"number":3,"title":"three","state":"open"}`),
			"GET /repos/acme/api/issues/8": reply(200, `{"number":8,"title":"eight","state":"closed"}`),
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[{"iid":3,"references":{"full":"grp/api#3"}},{"iid":8,"references":{"full":"GRP/api#8"}}]`),
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

func TestLinkedIssuesShouldIgnoreIssuesOfOtherRepositories(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":5,"repository":{"nameWithOwner":"other/repo"}}]}}}}}`),
			"GET /repos/acme/api/pulls/12": reply(200, `{"number":12,"body":"Fixes other/repo#5 and fixes #2"}`),
			"GET /repos/acme/api/issues/2": reply(200, `{"number":2,"title":"two","state":"open"}`),
			"GET /repos/acme/api/issues/5": reply(200, `{"number":5,"title":"not linked","state":"open"}`),
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[{"iid":5,"references":{"full":"other/proj#5"}}]`),
			"GET " + glProject + "/merge_requests/12":               reply(200, `{"iid":12,"description":"Closes other/proj#5 and closes #2"}`),
			"GET " + glProject + "/issues/2":                        reply(200, `{"iid":2,"title":"two","state":"opened"}`),
			"GET " + glProject + "/issues/5":                        reply(200, `{"iid":5,"title":"not linked","state":"opened"}`),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := tc.at(t, tc.routes)
			is, err := f.LinkedIssues(context.Background(), 12)
			if err != nil {
				t.Fatal(err)
			}
			if len(is) != 1 || is[0].Number != 2 {
				t.Fatalf("issue 5 of another repository is not this repository's #5; the body fallback still applies: %+v", is)
			}
		})
	}
}

func TestClosedInBodyShouldMatchWholeClosingKeywordsOnly(t *testing.T) {
	for body, want := range map[string][]int{
		"prefix #12, suffix #13, unresolved #14, bugfix #15": nil,
		"Fixes: #3":            {3},
		"closes   #4":          {4},
		"Resolved #5.":         {5},
		"fix #6x and fix #7":   {7},
		"Fixes other/repo#8":   nil,
		"(fixes #9) closes #9": {9},
	} {
		if got := closedInBody(body); !slices.Equal(got, want) {
			t.Errorf("closedInBody(%q) = %v, want %v", body, got, want)
		}
	}
}

func TestLinkedIssuesShouldFallBackToClosesKeywordsInBody(t *testing.T) {
	const body = "Fixes #3 and closes #3 again; resolves #4.\nCloses #5, fix #6, resolved #7, close #8. See #9."
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
		issue  func(n int) (string, string) // route and payload of issue n
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`),
			"GET /repos/acme/api/pulls/12": reply(200, `{"number":12,"body":`+strconv.Quote(body)+`}`),
		}, func(n int) (string, string) {
			return "GET /repos/acme/api/issues/" + strconv.Itoa(n), `{"number":` + strconv.Itoa(n) + `,"state":"open"}`
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[]`),
			"GET " + glProject + "/merge_requests/12":               reply(200, `{"iid":12,"description":`+strconv.Quote(body)+`}`),
		}, func(n int) (string, string) {
			return "GET " + glProject + "/issues/" + strconv.Itoa(n), `{"iid":` + strconv.Itoa(n) + `,"state":"opened"}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for n := 3; n <= 9; n++ {
				route, payload := tc.issue(n)
				tc.routes[route] = reply(200, payload)
			}
			f, _ := tc.at(t, tc.routes)
			is, err := f.LinkedIssues(context.Background(), 12)
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, i := range is {
				got = append(got, i.Number)
			}
			if !slices.Equal(got, []int{3, 4, 5, 6, 7}) {
				t.Fatalf("want the first 5 distinct closed issues of the body, in order, got %v", got)
			}
		})
	}
}

func TestThreadsShouldReturnEmptyForPRWithoutComments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
		read   string // an endpoint the threads must come from
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                          reply(200, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[]}}}}}`),
			"GET /repos/acme/api/issues/12/comments": reply(200, `[]`),
		}, "/repos/acme/api/issues/12/comments"},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/discussions": reply(200, `[]`),
		}, glProject + "/merge_requests/12/discussions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, api := tc.at(t, tc.routes)
			ts, err := f.Threads(context.Background(), 12)
			if err != nil || len(ts) != 0 {
				t.Fatalf("threads = %+v, err = %v", ts, err)
			}
			api.find(t, "GET", tc.read)
		})
	}
}

// forbidden is a read the forge rejects; 403 is final, so it is never retried.
var forbidden = reply(403, `{"message":"Resource not accessible by integration"}`)

// wantForbidden fails unless err is the forge's 403 and the read returned nothing.
func wantForbidden[T any](t *testing.T, got []T, err error) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || got != nil {
		t.Fatalf("a failed read must fail the whole call: got %+v, err %v", got, err)
	}
}

func TestLinkedIssuesShouldFailWhenALinkedIssueCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":3,"repository":{"nameWithOwner":"acme/api"}},{"number":8,"repository":{"nameWithOwner":"Acme/API"}}]}}}}}`),
			"GET /repos/acme/api/issues/3": reply(200, `{"number":3,"state":"open"}`),
			"GET /repos/acme/api/issues/8": forbidden,
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[{"iid":3,"references":{"full":"grp/api#3"}},{"iid":8,"references":{"full":"GRP/api#8"}}]`),
			"GET " + glProject + "/issues/3":                        reply(200, `{"iid":3,"state":"opened"}`),
			"GET " + glProject + "/issues/8":                        forbidden,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := tc.at(t, tc.routes)
			is, err := f.LinkedIssues(context.Background(), 12)
			wantForbidden(t, is, err)
		})
	}
}

func TestLinkedIssuesShouldFailWhenTheFallbackCannotReadThePR(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
		routes map[string]func(http.ResponseWriter, *http.Request)
	}{
		{"github", githubAt, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                reply(200, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[]}}}}}`),
			"GET /repos/acme/api/pulls/12": forbidden,
		}},
		{"gitlab", gitlabAt, map[string]func(http.ResponseWriter, *http.Request){
			"GET " + glProject + "/merge_requests/12/closes_issues": reply(200, `[]`),
			"GET " + glProject + "/merge_requests/12":               forbidden,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := tc.at(t, tc.routes)
			is, err := f.LinkedIssues(context.Background(), 12)
			wantForbidden(t, is, err)
		})
	}
}

// writeCase is a PR/MR write on one forge: the endpoints the fake serves.
type writeCase struct {
	name    string
	at      func(*testing.T, map[string]func(http.ResponseWriter, *http.Request), ...Option) (Forge, *fakeAPI)
	comment string // where a PR-level comment goes
	review  string // where a review with no inline comments goes
}

var writeCases = []writeCase{
	{"github", githubAt, "/repos/acme/api/issues/9/comments", "/repos/acme/api/pulls/9/reviews"},
	{"gitlab", gitlabAt, glProject + "/merge_requests/9/notes", glProject + "/merge_requests/9/notes"},
}

func TestPRCommentShouldPostOnThePullRequest(t *testing.T) {
	for _, wc := range writeCases {
		t.Run(wc.name, func(t *testing.T) {
			f, api := wc.at(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST " + wc.comment: reply(201, `{"id":1}`),
			})
			if err := f.PRComment(context.Background(), 9, "olá <!-- delamain:run=1 kind=summary key=ab -->"); err != nil {
				t.Fatal(err)
			}
			if n := len(api.requests()); n != 1 {
				t.Fatalf("requests = %+v, want one comment", api.requests())
			}
			if b := jsonBody(t, api.find(t, "POST", wc.comment))["body"]; b != "olá <!-- delamain:run=1 kind=summary key=ab -->" {
				t.Fatalf("body = %v", b)
			}
		})
	}
}

func TestPostReviewWithoutInlineCommentsShouldPostOnlyTheSummary(t *testing.T) {
	for _, wc := range writeCases {
		t.Run(wc.name, func(t *testing.T) {
			f, api := wc.at(t, map[string]func(http.ResponseWriter, *http.Request){
				"POST " + wc.review: reply(201, `{"id":1}`),
			})
			if err := f.PostReview(context.Background(), 9, ReviewOptions{HeadSHA: "h", BaseSHA: "b", StartSHA: "b", Summary: "all good"}); err != nil {
				t.Fatal(err)
			}
			rs := api.requests()
			if len(rs) != 1 {
				t.Fatalf("requests = %+v, want only the summary", rs)
			}
			b := jsonBody(t, api.find(t, "POST", wc.review))
			if b["body"] != "all good" {
				t.Fatalf("summary body = %v", b)
			}
			if cs, ok := b["comments"].([]any); ok && len(cs) > 0 {
				t.Fatalf("no inline comment was asked for: %v", cs)
			}
		})
	}
}

func TestReplyShouldPostIntoTheThread(t *testing.T) {
	t.Run("github review thread", func(t *testing.T) {
		f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                         reply(200, `{"data":{"node":{"comments":{"nodes":[{"databaseId":555}]}}}}`),
			"POST /repos/acme/api/pulls/9/comments": reply(201, `{"id":556}`),
		})
		if err := f.Reply(context.Background(), 9, "RT_abc", "thanks"); err != nil {
			t.Fatal(err)
		}
		q := jsonBody(t, api.find(t, "POST", "/graphql"))
		if vars, _ := q["variables"].(map[string]any); vars["id"] != "RT_abc" {
			t.Fatalf("the thread is looked up by its node id: %v", q)
		}
		b := jsonBody(t, api.find(t, "POST", "/repos/acme/api/pulls/9/comments"))
		if b["body"] != "thanks" || b["in_reply_to"] != float64(555) {
			t.Fatalf("reply = %v, want a reply to the thread's first comment", b)
		}
	})
	t.Run("github issue comment", func(t *testing.T) {
		f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /repos/acme/api/issues/9/comments": reply(201, `{"id":1}`),
		})
		if err := f.Reply(context.Background(), 9, "issue-comment:77", "thanks"); err != nil {
			t.Fatal(err)
		}
		if rs := api.requests(); len(rs) != 1 {
			t.Fatalf("requests = %+v, want one PR comment", rs)
		}
		if b := jsonBody(t, api.find(t, "POST", "/repos/acme/api/issues/9/comments"))["body"]; b != "thanks" {
			t.Fatalf("body = %v", b)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		path := glProject + "/merge_requests/9/discussions/d1/notes"
		f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST " + path: reply(201, `{"id":1}`),
		})
		if err := f.Reply(context.Background(), 9, "d1", "thanks"); err != nil {
			t.Fatal(err)
		}
		if b := jsonBody(t, api.find(t, "POST", path))["body"]; b != "thanks" {
			t.Fatalf("body = %v", b)
		}
	})
}

func TestRelabelPRShouldAddAndRemoveLabels(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /repos/acme/api/labels":                            reply(422, ghLabelExists),
			"POST /repos/acme/api/issues/9/labels":                   reply(200, `[]`),
			"DELETE /repos/acme/api/issues/9/labels/delamain:review": reply(200, `[]`),
		})
		if err := f.RelabelPR(context.Background(), 9, []string{"delamain:reviewed"}, []string{"delamain:review"}); err != nil {
			t.Fatal(err)
		}
		if n := posts(api, "/repos/acme/api/labels"); n != 2 {
			t.Fatalf("labels ensured = %d, want the added and the removed one", n)
		}
		var added []string
		if err := json.Unmarshal([]byte(api.find(t, "POST", "/repos/acme/api/issues/9/labels").body), &added); err != nil || !slices.Equal(added, []string{"delamain:reviewed"}) {
			t.Fatalf("added = %v %v", added, err)
		}
		api.find(t, "DELETE", "/repos/acme/api/issues/9/labels/delamain:review")
	})
	t.Run("gitlab", func(t *testing.T) {
		path := glProject + "/merge_requests/9"
		f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"PUT " + path: reply(200, `{"iid":9}`),
		})
		if err := f.RelabelPR(context.Background(), 9, []string{"delamain:reviewed", "x"}, []string{"delamain:review"}); err != nil {
			t.Fatal(err)
		}
		if b := jsonBody(t, api.find(t, "PUT", path)); b["add_labels"] != "delamain:reviewed,x" || b["remove_labels"] != "delamain:review" {
			t.Fatalf("update body = %v", b)
		}

		f, api = gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"PUT " + path: reply(200, `{"iid":9}`),
		})
		if err := f.RelabelPR(context.Background(), 9, nil, []string{"delamain:review"}); err != nil {
			t.Fatal(err)
		}
		if b := jsonBody(t, api.find(t, "PUT", path)); b["remove_labels"] != "delamain:review" {
			t.Fatalf("update body = %v", b)
		} else if _, ok := b["add_labels"]; ok {
			t.Fatalf("an empty addition must not be sent: %v", b)
		}
	})
}

// A forge may quote what it rejected; the bodies delamain posts must still stay out of errors,
// even when they are longer than the error message itself.
func TestWriteMethodsShouldNeverPutBodiesInErrors(t *testing.T) {
	sizes := map[string]string{"short": "", "long": strings.Repeat(" and more", maxErrorLen)}
	for _, repo := range []Repo{{GitHub, "github.com", "acme/api"}, {GitLab, "gitlab.com", "grp/api"}} {
		for size, pad := range sizes {
			for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
				t.Run(string(repo.Kind)+"/"+size+"/"+strconv.Itoa(status), func(t *testing.T) {
					srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
						b, _ := io.ReadAll(r.Body)
						if r.URL.Path == "/graphql" {
							reply(200, `{"data":{"node":{"comments":{"nodes":[{"databaseId":5}]}}}}`)(w, r)
							return
						}
						msg, _ := json.Marshal(map[string]string{"message": "rejected " + string(b)})
						reply(status, string(msg))(w, r)
					})
					f, err := New(repo, "tok", WithBaseURL(srv+"/"), WithBackoff(1))
					if err != nil {
						t.Fatal(err)
					}
					ctx := context.Background()
					writes := map[string]func() error{
						"PostReview": func() error {
							return f.PostReview(ctx, 9, ReviewOptions{HeadSHA: "h", BaseSHA: "b", StartSHA: "b", Summary: "BODY-summary" + pad,
								Comments: []InlineComment{{Path: "a.go", Line: 1, Body: "BODY-inline" + pad}}})
						},
						"PRComment":           func() error { return f.PRComment(ctx, 9, "BODY-comment"+pad) },
						"Reply":               func() error { return f.Reply(ctx, 9, "RT_1", "BODY-reply"+pad) },
						"Reply issue comment": func() error { return f.Reply(ctx, 9, "issue-comment:1", "BODY-reply"+pad) },
					}
					for name, write := range writes {
						err := write()
						if err == nil {
							t.Errorf("%s: the forge rejected it, want an error", name)
							continue
						}
						if strings.Contains(err.Error(), "BODY-") {
							t.Errorf("%s: error leaks a body: %v", name, err)
						}
						if !strings.Contains(err.Error(), strconv.Itoa(status)) {
							t.Errorf("%s: error must keep the status: %v", name, err)
						}
					}
				})
			}
		}
	}
}

// roundTripFunc is a transport that never reaches a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWriteMethodsShouldKeepTheErrorChainWhenHidingBodies(t *testing.T) {
	// A transport that quotes the body it failed to send, like some proxies do.
	quoting := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return nil, fmt.Errorf("proxy refused %s: %w", b, context.DeadlineExceeded)
	})}
	for _, repo := range []Repo{{GitHub, "github.com", "acme/api"}, {GitLab, "gitlab.com", "grp/api"}} {
		f, err := New(repo, "tok", WithBaseURL("http://forge.invalid/"), WithBackoff(1), WithHTTPClient(quoting))
		if err != nil {
			t.Fatal(err)
		}
		err = f.PRComment(context.Background(), 9, "BODY-comment")
		if err == nil || strings.Contains(err.Error(), "BODY-") {
			t.Errorf("%s: error = %v, want one without the body", repo.Kind, err)
		}
		if _, ok := errors.AsType[*url.Error](err); !ok || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: hiding the body must keep the wrapped error: %v", repo.Kind, err)
		}
	}
}
