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
	"testing"
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
