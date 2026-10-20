package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// gitlabAt serves routes as gitlab.com and returns a client for grp/api pointed at it.
func gitlabAt(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request), opts ...Option) (Forge, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{routes: routes}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	f, err := New(Repo{GitLab, "gitlab.com", "grp/api"}, "glpat-tok", append([]Option{WithBaseURL(srv.URL), WithBackoff(1)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return f, api
}

const glProject = "/api/v4/projects/grp%2Fapi"

func TestGitLabShouldReadIssue(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7": reply(200, `{"iid":7,"title":"add x","description":"details","web_url":"https://gitlab.com/grp/api/-/issues/7","state":"opened","labels":["delamain:ready","bug"]}`),
	})
	is, err := f.Issue(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if is.Number != 7 || is.Title != "add x" || is.Body != "details" || is.URL != "https://gitlab.com/grp/api/-/issues/7" || !is.Open || strings.Join(is.Labels, ",") != "delamain:ready,bug" {
		t.Fatalf("issue = %+v", is)
	}
	if got := api.find(t, "GET", glProject+"/issues/7").header.Get("Private-Token"); got != "glpat-tok" {
		t.Fatalf("token header = %q", got)
	}
}

func TestGitLabShouldReadClosedIssueAsNotOpen(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7": reply(200, `{"iid":7,"title":"t","state":"closed"}`),
	})
	is, err := f.Issue(context.Background(), 7)
	if err != nil || is.Open {
		t.Fatalf("closed issue must not be open: %+v %v", is, err)
	}
}

func TestGitLabListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues": reply(200, `[{"iid":9,"title":"nine","state":"opened"},{"iid":5,"title":"five","state":"opened"}]`),
	})
	is, err := f.ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 || is[0].Number != 5 || is[1].Number != 9 {
		t.Fatalf("want issues 5 then 9, got %+v", is)
	}
	q, _ := url.ParseQuery(api.find(t, "GET", glProject+"/issues").query)
	if q.Get("state") != "opened" || q.Get("labels") != "delamain:ready" || q.Get("per_page") != "100" {
		t.Fatalf("must list opened issues by label, up to 100: %v", q)
	}
}

func TestGitLabOpenPRShouldReturnNilWhenNoMRIsOpen(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/merge_requests": reply(200, `[]`),
	})
	pr, err := f.OpenPR(context.Background(), "feat/x")
	if err != nil || pr != nil {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	q, _ := url.ParseQuery(api.find(t, "GET", glProject+"/merge_requests").query)
	if q.Get("source_branch") != "feat/x" || q.Get("state") != "opened" {
		t.Fatalf("must filter opened MRs by source branch: %v", q)
	}
}

func TestGitLabOpenPRShouldReturnTheExistingMR(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/merge_requests": reply(200, `[{"iid":3,"web_url":"https://gitlab.com/grp/api/-/merge_requests/3","draft":true}]`),
	})
	pr, err := f.OpenPR(context.Background(), "feat/x")
	if err != nil || pr == nil || *pr != (PR{Number: 3, URL: "https://gitlab.com/grp/api/-/merge_requests/3", Draft: true}) {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
}

func TestGitLabSelfHostedShouldUseApiV4BaseURL(t *testing.T) {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v4/projects/team%2Fsub%2Fapi/merge_requests": reply(200, `[]`),
	}}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	f, err := New(Repo{GitLab, host, "team/sub/api"}, "tok", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.OpenPR(context.Background(), "feat/x"); err != nil {
		t.Fatalf("self-hosted API lives under https://<host>/api/v4: %v (requests %+v)", err, api.requests())
	}
}

func TestGitLabShouldCreateDraftMR(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + glProject + "/merge_requests": reply(201, `{"iid":15,"web_url":"https://gitlab.com/grp/api/-/merge_requests/15","draft":true}`),
	})
	pr, err := f.CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if pr != (PR{Number: 15, URL: "https://gitlab.com/grp/api/-/merge_requests/15", Draft: true}) {
		t.Fatalf("mr = %+v", pr)
	}
	b := jsonBody(t, api.find(t, "POST", glProject+"/merge_requests"))
	if b["source_branch"] != "fix/7-add" || b["target_branch"] != "main" || b["title"] != "Draft: fix: add" || b["description"] != "Closes #7" {
		t.Fatalf("create body = %v", b)
	}
}

func TestGitLabShouldOpenReadyMRWhenNotDraft(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + glProject + "/merge_requests": reply(201, `{"iid":1,"web_url":"https://gitlab.com/grp/api/-/merge_requests/1","draft":false}`),
	})
	pr, err := f.CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "feat: x"})
	if err != nil || pr.Draft {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	if title := jsonBody(t, api.find(t, "POST", glProject+"/merge_requests"))["title"]; title != "feat: x" {
		t.Fatalf("approved work must not be opened as draft, title = %v", title)
	}
}

func TestGitLabCommentShouldSendTheBodyInTheRequestBody(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + glProject + "/issues/3/notes": reply(201, `{"id":1}`),
	})
	if err := f.Comment(context.Background(), 3, "status: olá"); err != nil {
		t.Fatal(err)
	}
	r := api.find(t, "POST", glProject+"/issues/3/notes")
	if jsonBody(t, r)["body"] != "status: olá" || strings.Contains(r.query, "status") {
		t.Fatalf("note must travel in the request body, never the URL: %+v", r)
	}
}

func TestGitLabShouldRelabelWithoutEmptyLabels(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"PUT " + glProject + "/issues/3": reply(200, `{"iid":3}`),
	})
	if err := f.Relabel(context.Background(), 3, []string{"a", "b"}, nil); err != nil {
		t.Fatal(err)
	}
	b := jsonBody(t, api.find(t, "PUT", glProject+"/issues/3"))
	if b["add_labels"] != "a,b" {
		t.Fatalf("update body = %v", b)
	}
	if _, ok := b["remove_labels"]; ok {
		t.Fatalf("an empty removal must not be sent: %v", b)
	}
}
