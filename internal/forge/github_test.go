package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// request is one call a fake forge server received.
type request struct {
	method, path, query string
	header              http.Header
	body                string
}

// fakeAPI is an httptest forge: routes answer "METHOD /path" with a status and a JSON body.
type fakeAPI struct {
	mu     sync.Mutex
	reqs   []request
	routes map[string]func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, request{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(b)})
	h := f.routes[r.Method+" "+r.URL.EscapedPath()]
	f.mu.Unlock()
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"Not Found"}`)
		return
	}
	h(w, r)
}

func (f *fakeAPI) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.reqs...)
}

func (f *fakeAPI) find(t *testing.T, method, path string) request {
	t.Helper()
	for _, r := range f.requests() {
		if r.method == method && r.path == path {
			return r
		}
	}
	t.Fatalf("no %s %s in %+v", method, path, f.requests())
	return request{}
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// githubAt serves routes as github.com and returns a client for acme/api pointed at it.
func githubAt(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request), opts ...Option) (Forge, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{routes: routes}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	f, err := New(Repo{GitHub, "github.com", "acme/api"}, "tok", append([]Option{WithBaseURL(srv.URL + "/"), WithBackoff(1)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return f, api
}

const ghIssue7 = `{"number":7,"title":"add x","body":"details","html_url":"https://github.com/acme/api/issues/7","state":"open","labels":[{"name":"delamain:ready"},{"name":"bug"}]}`

func TestGitHubShouldReadIssue(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7": reply(200, ghIssue7),
	})
	is, err := f.Issue(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{Number: 7, Title: "add x", Body: "details", URL: "https://github.com/acme/api/issues/7", Labels: []string{"delamain:ready", "bug"}, Open: true}
	if is.Number != want.Number || is.Title != want.Title || is.Body != want.Body || is.URL != want.URL || !is.Open || strings.Join(is.Labels, ",") != "delamain:ready,bug" {
		t.Fatalf("issue = %+v, want %+v", is, want)
	}
}

func TestGitHubShouldReadClosedIssueAsNotOpen(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7": reply(200, `{"number":7,"title":"t","state":"closed"}`),
	})
	is, err := f.Issue(context.Background(), 7)
	if err != nil || is.Open {
		t.Fatalf("closed issue must not be open: %+v %v", is, err)
	}
}

func TestGitHubListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues": reply(200, `[
			{"number":9,"title":"nine","state":"open"},
			{"number":4,"title":"a PR","state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/4"}},
			{"number":5,"title":"five","state":"open"}]`),
	})
	is, err := f.ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 || is[0].Number != 5 || is[1].Number != 9 {
		t.Fatalf("want issues 5 then 9 without the PR, got %+v", is)
	}
	q, _ := url.ParseQuery(api.find(t, "GET", "/repos/acme/api/issues").query)
	if q.Get("state") != "open" || q.Get("labels") != "delamain:ready" || q.Get("per_page") != "100" {
		t.Fatalf("must list open issues by label, up to 100: %v", q)
	}
}

func TestGitHubOpenPRShouldReturnNilWhenNoneIsOpen(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/pulls": reply(200, `[]`),
	})
	pr, err := f.OpenPR(context.Background(), "feat/x")
	if err != nil || pr != nil {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	q, _ := url.ParseQuery(api.find(t, "GET", "/repos/acme/api/pulls").query)
	if q.Get("head") != "acme:feat/x" || q.Get("state") != "open" {
		t.Fatalf("must filter open PRs by owner:head: %v", q)
	}
}

func TestGitHubOpenPRShouldReturnTheExistingPR(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/pulls": reply(200, `[{"number":9,"html_url":"https://github.com/acme/api/pull/9","draft":true}]`),
	})
	pr, err := f.OpenPR(context.Background(), "feat/x")
	if err != nil || pr == nil || *pr != (PR{Number: 9, URL: "https://github.com/acme/api/pull/9", Draft: true}) {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
}

func TestGitHubEnterpriseShouldUseApiV3BaseURL(t *testing.T) {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v3/repos/team/api/pulls": reply(200, `[]`),
	}}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	f, err := New(Repo{GitHub, host, "team/api"}, "tok", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.OpenPR(context.Background(), "feat/x"); err != nil {
		t.Fatalf("enterprise API lives under https://<host>/api/v3/: %v (requests %+v)", err, api.requests())
	}
}

func TestGitHubShouldSendDelamainUserAgentAndToken(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7": reply(200, ghIssue7),
	}, WithUserAgent("delamain/v1.2.3"))
	if _, err := f.Issue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	r := api.find(t, "GET", "/repos/acme/api/issues/7")
	if r.header.Get("User-Agent") != "delamain/v1.2.3" || r.header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("headers = %v", r.header)
	}
}

func TestGitHubShouldReportAPIErrorsWithStatusAndMessage(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7": reply(404, `{"message":"Not Found"}`),
	})
	_, err := f.Issue(context.Background(), 7)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("want an APIError with status 404, got %v", err)
	}
	if err.Error() != "github issue: 404 Not Found" {
		t.Fatalf("error = %q", err.Error())
	}
}

// jsonBody decodes a recorded request body.
func jsonBody(t *testing.T, r request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("body %q: %v", r.body, err)
	}
	return m
}

func TestGitHubShouldCreateDraftPR(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/pulls": reply(201, `{"number":42,"html_url":"https://github.com/acme/api/pull/42","draft":true}`),
	})
	pr, err := f.CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PR{Number: 42, URL: "https://github.com/acme/api/pull/42", Draft: true}) {
		t.Fatalf("pr = %+v", pr)
	}
	b := jsonBody(t, api.find(t, "POST", "/repos/acme/api/pulls"))
	if b["head"] != "fix/7-add" || b["base"] != "main" || b["title"] != "fix: add" || b["body"] != "Closes #7" || b["draft"] != true {
		t.Fatalf("create body = %v", b)
	}
}

func TestGitHubShouldOpenReadyPRWhenNotDraft(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/pulls": reply(201, `{"number":1,"html_url":"https://github.com/acme/api/pull/1","draft":false}`),
	})
	pr, err := f.CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "feat: x"})
	if err != nil || pr.Draft {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	if d, ok := jsonBody(t, api.find(t, "POST", "/repos/acme/api/pulls"))["draft"]; ok && d != false {
		t.Fatal("approved work must not be opened as draft")
	}
}

func TestGitHubCommentShouldPostTheBodyAsJSON(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/issues/7/comments": reply(201, `{"id":1}`),
	})
	if err := f.Comment(context.Background(), 7, "olá & \"aspas\""); err != nil {
		t.Fatal(err)
	}
	r := api.find(t, "POST", "/repos/acme/api/issues/7/comments")
	if jsonBody(t, r)["body"] != "olá & \"aspas\"" || strings.Contains(r.query, "ol") {
		t.Fatalf("comment must travel in the JSON body: %+v", r)
	}
}

const ghLabelExists = `{"message":"Validation Failed","errors":[{"resource":"Label","code":"already_exists","field":"name"}]}`

func TestGitHubShouldEnsureLabelsExistBeforeRelabeling(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/labels":                           reply(422, ghLabelExists),
		"POST /repos/acme/api/issues/7/labels":                  reply(200, `[]`),
		"DELETE /repos/acme/api/issues/7/labels/delamain:ready": reply(200, `[]`),
	})
	if err := f.Relabel(context.Background(), 7, []string{"delamain:in-progress"}, []string{"delamain:ready"}); err != nil {
		t.Fatalf("an existing label is not an error: %v", err)
	}
	var created []string
	for _, r := range api.requests() {
		if r.method == "POST" && r.path == "/repos/acme/api/labels" {
			b := jsonBody(t, r)
			created = append(created, b["name"].(string))
			if b["color"] != "5319e7" || b["description"] != "delamain" {
				t.Errorf("label body = %v", b)
			}
		}
	}
	if strings.Join(created, ",") != "delamain:in-progress,delamain:ready" {
		t.Fatalf("labels ensured = %v, want added and removed", created)
	}
	var added []string
	if err := json.Unmarshal([]byte(api.find(t, "POST", "/repos/acme/api/issues/7/labels").body), &added); err != nil || strings.Join(added, ",") != "delamain:in-progress" {
		t.Fatalf("added = %v %v", added, err)
	}
	api.find(t, "DELETE", "/repos/acme/api/issues/7/labels/delamain:ready")
}

func TestGitHubRelabelShouldIgnoreRemovingALabelTheIssueDoesNotHave(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/labels":                           reply(201, `{"name":"x"}`),
		"DELETE /repos/acme/api/issues/7/labels/delamain:ready": reply(404, `{"message":"Label does not exist"}`),
	})
	if err := f.Relabel(context.Background(), 7, nil, []string{"delamain:ready"}); err != nil {
		t.Fatalf("removing an absent label is a no-op: %v", err)
	}
}

func TestGitHubRelabelShouldPropagateRealLabelErrors(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/labels": reply(403, `{"message":"Resource not accessible by integration"}`),
	})
	err := f.Relabel(context.Background(), 7, []string{"x"}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 403 {
		t.Fatalf("a permission error must not be swallowed: %v", err)
	}
	for _, r := range api.requests() {
		if r.path == "/repos/acme/api/issues/7/labels" {
			t.Fatal("must not relabel after failing to ensure the label")
		}
	}
}

func TestGitHubWhoamiShouldReturnTheLoginFromGetUser(t *testing.T) {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /user": reply(200, `{"login":"delamain-bot","id":1}`),
	}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	login, err := Whoami(context.Background(), Repo{GitHub, "github.com", "acme/api"}, "tok", WithBaseURL(srv.URL+"/"))
	if err != nil || login != "delamain-bot" {
		t.Fatalf("login = %q, err = %v", login, err)
	}
	if got := api.find(t, "GET", "/user").header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("GET /user must carry the token, got %q", got)
	}
}

func TestGitHubWhoamiShouldReportARejectedToken(t *testing.T) {
	srv := httptest.NewServer(&fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /user": reply(401, `{"message":"Bad credentials"}`),
	}})
	t.Cleanup(srv.Close)
	_, err := Whoami(context.Background(), Repo{GitHub, "github.com", "acme/api"}, "tok", WithBaseURL(srv.URL+"/"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("want a 401 APIError, got %v", err)
	}
}

func TestGitHubPRViewShouldParseRefsShasAndFork(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/pulls/5": reply(200, `{"number":5,"html_url":"https://github.com/acme/api/pull/5","title":"feat: x","body":"Closes #2",
			"user":{"login":"ana"},"draft":true,"state":"open",
			"base":{"ref":"main","sha":"b1","repo":{"full_name":"acme/api"}},
			"head":{"ref":"feat/x","sha":"h1","repo":{"full_name":"acme/api"}}}`),
		"GET /repos/acme/api/pulls/6": reply(200, `{"number":6,"state":"closed",
			"base":{"ref":"main","sha":"b2","repo":{"full_name":"acme/api"}},
			"head":{"ref":"patch-1","sha":"h2","repo":{"full_name":"bob/api"}}}`),
		"GET /repos/acme/api/pulls/7": reply(200, `{"number":7,"state":"open",
			"base":{"ref":"main","sha":"b3","repo":{"full_name":"acme/api"}},
			"head":{"ref":"patch-2","sha":"h3","repo":null}}`),
	})
	ctx := context.Background()
	pr, err := f.PRView(ctx, 5)
	want := PRDetail{Number: 5, URL: "https://github.com/acme/api/pull/5", Title: "feat: x", Body: "Closes #2", Author: "ana",
		Draft: true, Open: true, BaseRef: "main", HeadRef: "feat/x", BaseSHA: "b1", HeadSHA: "h1", StartSHA: "b1"}
	if err != nil || pr != want {
		t.Fatalf("pr = %+v, err = %v; want %+v", pr, err, want)
	}

	pr, err = f.PRView(ctx, 6)
	if err != nil || !pr.Fork || pr.Open || pr.HeadRef != "patch-1" || pr.HeadSHA != "h2" || pr.StartSHA != "b2" {
		t.Fatalf("a head in another repository is a fork: %+v %v", pr, err)
	}

	pr, err = f.PRView(ctx, 7)
	if err != nil || !pr.Fork {
		t.Fatalf("a head whose repository was deleted is a fork: %+v %v", pr, err)
	}
}

// at parses an RFC 3339 time for expectations.
func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestGitHubThreadsShouldMergeReviewThreadsAndIssueComments(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"RT_2","isResolved":true,"path":"a.go","line":10,"comments":{"nodes":[
				{"databaseId":201,"author":{"login":"ana"},"body":"fix this","createdAt":"2026-01-01T10:00:00Z"},
				{"databaseId":202,"author":null,"body":"done","createdAt":"2026-01-01T12:00:00Z"}]}},
			{"id":"RT_1","isResolved":false,"path":"b.go","line":null,"comments":{"nodes":[
				{"databaseId":101,"author":{"login":"bob"},"body":"why?","createdAt":"2026-01-01T08:00:00Z"}]}}]}}}}}`),
		"GET /repos/acme/api/issues/12/comments": reply(200, `[
			{"id":301,"user":{"login":"carol"},"body":"lgtm","created_at":"2026-01-01T09:00:00Z"},
			{"id":302,"user":{"login":"coderabbitai[bot]","type":"Bot"},"body":"walkthrough","created_at":"2026-01-01T09:30:00Z"}]`),
	})
	ts, err := f.Threads(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []Thread{
		{ID: "RT_1", Path: "b.go", Comments: []Comment{{ID: "101", Author: "bob", Body: "why?", CreatedAt: at(t, "2026-01-01T08:00:00Z")}}},
		{ID: "issue-comment:301", Comments: []Comment{{ID: "301", Author: "carol", Body: "lgtm", CreatedAt: at(t, "2026-01-01T09:00:00Z")}}},
		{ID: "issue-comment:302", Comments: []Comment{{ID: "302", Author: "coderabbitai[bot]", Body: "walkthrough", CreatedAt: at(t, "2026-01-01T09:30:00Z"), Bot: true}}},
		{ID: "RT_2", Path: "a.go", Line: 10, Resolved: true, Comments: []Comment{
			{ID: "201", Author: "ana", Body: "fix this", CreatedAt: at(t, "2026-01-01T10:00:00Z")},
			{ID: "202", Body: "done", CreatedAt: at(t, "2026-01-01T12:00:00Z")},
		}},
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("threads =\n%+v\nwant, by first comment,\n%+v", ts, want)
	}
	var sent struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	r := api.find(t, "POST", "/graphql")
	if err := json.Unmarshal([]byte(r.body), &sent); err != nil || !strings.Contains(sent.Query, "reviewThreads(first:100)") ||
		sent.Variables["owner"] != "acme" || sent.Variables["name"] != "api" || sent.Variables["number"] != float64(12) {
		t.Fatalf("graphql request = %s (%v)", r.body, err)
	}
	if q, _ := url.ParseQuery(api.find(t, "GET", "/repos/acme/api/issues/12/comments").query); q.Get("per_page") != "100" {
		t.Fatalf("must read up to 100 PR comments: %v", q)
	}
}

func TestGitHubListPRsShouldFailWhenAPRCannotBeRead(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues": reply(200, `[
			{"number":5,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/5"}},
			{"number":9,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/9"}}]`),
		"GET /repos/acme/api/pulls/5": reply(200, `{"number":5,"state":"open"}`),
		"GET /repos/acme/api/pulls/9": forbidden,
	})
	prs, err := f.ListPRs(context.Background(), "delamain:review")
	wantForbidden(t, prs, err)
}

func TestGitHubThreadsShouldFailWhenPRCommentsCannotBeRead(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
			{"id":"RT_1","path":"a.go","line":1,"comments":{"nodes":[{"databaseId":1,"body":"x","createdAt":"2026-01-01T08:00:00Z"}]}}]}}}}}`),
		"GET /repos/acme/api/issues/12/comments": forbidden,
	})
	ts, err := f.Threads(context.Background(), 12)
	wantForbidden(t, ts, err)
}

func TestGitHubPostReviewShouldSendOneCommentEventWithInlineComments(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /repos/acme/api/pulls/9/reviews": reply(200, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "head1", BaseSHA: "base1", StartSHA: "base1", Summary: "summary <!-- m -->",
		Comments: []InlineComment{{Path: "a.go", Line: 12, Body: "nil deref"}, {Path: "b/c.go", Line: 3, Body: "typo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(api.requests()); n != 1 {
		t.Fatalf("requests = %d, want one review: %+v", n, api.requests())
	}
	b := jsonBody(t, api.find(t, "POST", "/repos/acme/api/pulls/9/reviews"))
	want := map[string]any{
		"commit_id": "head1", "event": "COMMENT", "body": "summary <!-- m -->",
		"comments": []any{
			map[string]any{"path": "a.go", "line": float64(12), "side": "RIGHT", "body": "nil deref"},
			map[string]any{"path": "b/c.go", "line": float64(3), "side": "RIGHT", "body": "typo"},
		},
	}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("review body = %v\nwant %v", b, want)
	}
}

func TestGitHubPostReviewShouldNeverSendApproveOrRequestChanges(t *testing.T) {
	for name, o := range map[string]ReviewOptions{
		"inline":        {HeadSHA: "h", Summary: "s", Comments: []InlineComment{{Path: "a.go", Line: 1, Body: "APPROVE"}}},
		"summary only":  {HeadSHA: "h", Summary: "REQUEST_CHANGES"},
		"empty summary": {HeadSHA: "h"},
	} {
		f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /repos/acme/api/pulls/9/reviews": reply(200, `{"id":1}`),
		})
		if err := f.PostReview(context.Background(), 9, o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rs := api.requests()
		if len(rs) == 0 {
			t.Fatalf("%s: no review was sent", name)
		}
		for _, r := range rs {
			if r.method != http.MethodPost || r.path != "/repos/acme/api/pulls/9/reviews" {
				t.Errorf("%s: unexpected call %s %s", name, r.method, r.path)
			}
			if ev := jsonBody(t, r)["event"]; ev != "COMMENT" {
				t.Errorf("%s: event = %v, want COMMENT", name, ev)
			}
		}
	}
}

func TestGitHubReplyShouldFailWhenTheThreadHasNoComments(t *testing.T) {
	for name, data := range map[string]string{
		"no comments":    `{"data":{"node":{"comments":{"nodes":[]}}}}`,
		"missing thread": `{"data":{"node":null}}`,
	} {
		f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
			"POST /graphql":                         reply(200, data),
			"POST /repos/acme/api/pulls/9/comments": reply(201, `{"id":1}`),
		})
		if err := f.Reply(context.Background(), 9, "RT_gone", "thanks"); err == nil {
			t.Errorf("%s: a thread with no comment to reply to must be an error", name)
		}
		if n := posts(api, "/repos/acme/api/pulls/9/comments"); n != 0 {
			t.Errorf("%s: nothing may be posted, got %d", name, n)
		}
	}
}

func TestResolveThreadShouldBeNoOpForIssueComments(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){})
	if err := f.ResolveThread(context.Background(), 9, "issue-comment:77"); err != nil {
		t.Fatalf("a PR-level comment has nothing to resolve: %v", err)
	}
	if rs := api.requests(); len(rs) != 0 {
		t.Fatalf("requests = %+v, want none", rs)
	}
}

func TestGitHubResolveThreadShouldSendResolveReviewThreadMutation(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":{"resolveReviewThread":{"thread":{"isResolved":true}}}}`),
	})
	if err := f.ResolveThread(context.Background(), 9, "RT_abc"); err != nil {
		t.Fatal(err)
	}
	r := api.find(t, "POST", "/graphql")
	q := strings.TrimSpace(jsonQuery(t, r))
	if !strings.HasPrefix(q, "mutation") || !strings.Contains(q, "resolveReviewThread") {
		t.Fatalf("query = %q, want the resolveReviewThread mutation", q)
	}
	if vars, _ := jsonBody(t, r)["variables"].(map[string]any); vars["threadId"] != "RT_abc" {
		t.Fatalf("variables = %v, want threadId RT_abc", vars)
	}

	f, _ = githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":null,"errors":[{"message":"Could not resolve to a node"}]}`),
	})
	if err := f.ResolveThread(context.Background(), 9, "RT_gone"); err == nil {
		t.Fatal("a mutation GraphQL rejects must be an error")
	}
}

func TestGitHubBlockersShouldListOpenAndClosedBlockers(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7/dependencies/blocked_by": reply(200, `[
			{"number":3,"state":"open","repository_url":"https://api.github.com/repos/acme/api"},
			{"number":5,"state":"closed","repository_url":"https://api.github.com/repos/acme/api"}]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "#3", Open: true}, {Number: 5, Ref: "#5", Open: false}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("blockers = %+v, want %+v", bs, want)
	}
	if q := api.find(t, http.MethodGet, "/repos/acme/api/issues/7/dependencies/blocked_by").query; q != "per_page=100" {
		t.Fatalf("query = %q, want per_page=100", q)
	}
}

func TestGitHubBlockersShouldNameBlockersFromAnotherRepository(t *testing.T) {
	// #3 of evil/api must not be mistaken for #3 of acme/api.
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7/dependencies/blocked_by": reply(200, `[
			{"number":3,"state":"open","repository_url":"https://api.github.com/repos/evil/api","repository":{"full_name":"evil/api"}},
			{"number":3,"state":"open","repository_url":"https://api.github.com/repos/other/lib"},
			{"number":4,"state":"closed","repository_url":"https://api.github.com/repos/Acme/API","repository":{"full_name":"Acme/API"}}]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "evil/api#3", Open: true}, {Number: 3, Ref: "other/lib#3", Open: true}, {Number: 4, Ref: "#4", Open: false}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("blockers = %+v, want %+v", bs, want)
	}
}

func TestGitHubBlockersShouldFollowPaginationPastOneHundredBlockers(t *testing.T) {
	// A blocker left on a later page would let a blocked issue run.
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7/dependencies/blocked_by": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				io.WriteString(w, `[{"number":9,"state":"open","repository_url":"https://api.github.com/repos/acme/api"}]`)
				return
			}
			w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=2&per_page=100>; rel="next"`)
			io.WriteString(w, `[{"number":3,"state":"closed","repository_url":"https://api.github.com/repos/acme/api"}]`)
		},
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "#3", Open: false}, {Number: 9, Ref: "#9", Open: true}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("both pages' blockers must come back: %+v", bs)
	}
	var calls int
	for _, rq := range api.requests() {
		if rq.method == http.MethodGet && strings.HasSuffix(rq.path, "/dependencies/blocked_by") {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("the second page must be fetched exactly once, got %d calls", calls)
	}
}

func TestGitHubBlockersShouldReturnEmptyWhenThereAreNone(t *testing.T) {
	f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/issues/7/dependencies/blocked_by": reply(200, `[]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil || len(bs) != 0 {
		t.Fatalf("an issue without blockers must have none: %+v %v", bs, err)
	}
}

func TestGitHubBlockersShouldPropagateAPIErrors(t *testing.T) {
	const path = "/repos/acme/api/issues/7/dependencies/blocked_by"
	for _, tc := range []struct {
		name  string
		route func(http.ResponseWriter, *http.Request)
	}{
		{"first page", forbidden},
		{"later page", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				forbidden(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", `<http://`+r.Host+path+`?page=2&per_page=100>; rel="next"`)
			io.WriteString(w, `[{"number":3,"state":"open","repository_url":"https://api.github.com/repos/acme/api"}]`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){"GET " + path: tc.route})
			bs, err := f.Blockers(context.Background(), 7)
			wantForbidden(t, bs, err)
		})
	}
}

func TestGitHubListOpenPRsShouldReturnOpenPRsWithoutLabelFilter(t *testing.T) {
	f, api := githubAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /repos/acme/api/pulls": reply(200, `[
			{"number":9,"html_url":"https://github.com/acme/api/pull/9","title":"nine","body":"b9","user":{"login":"bob"},"draft":true,"state":"open",
				"base":{"ref":"main","sha":"b9","repo":{"full_name":"acme/api"}},"head":{"ref":"patch-9","sha":"h9","repo":{"full_name":"bob/api"}}},
			{"number":5,"html_url":"https://github.com/acme/api/pull/5","title":"five","user":{"login":"ana"},"state":"open",
				"base":{"ref":"main","sha":"b5","repo":{"full_name":"acme/api"}},"head":{"ref":"feat/five","sha":"h5","repo":{"full_name":"acme/api"}}}]`),
	})
	prs, err := f.ListOpenPRs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []PRDetail{
		{Number: 9, URL: "https://github.com/acme/api/pull/9", Title: "nine", Body: "b9", Author: "bob", Draft: true, Open: true, Fork: true,
			BaseRef: "main", HeadRef: "patch-9", BaseSHA: "b9", HeadSHA: "h9", StartSHA: "b9"},
		{Number: 5, URL: "https://github.com/acme/api/pull/5", Title: "five", Author: "ana", Open: true,
			BaseRef: "main", HeadRef: "feat/five", BaseSHA: "b5", HeadSHA: "h5", StartSHA: "b5"},
	}
	if !reflect.DeepEqual(prs, want) {
		t.Fatalf("want every open PR newest first, as the forge sorts them:\n got %+v\nwant %+v", prs, want)
	}
	q, _ := url.ParseQuery(api.find(t, "GET", "/repos/acme/api/pulls").query)
	if q.Get("state") != "open" || q.Get("sort") != "created" || q.Get("direction") != "desc" || q.Get("per_page") != "10" || q.Has("labels") {
		t.Fatalf("must list open PRs newest first, with no label filter: %v", q)
	}
}
