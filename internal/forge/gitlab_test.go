package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func TestGitLabWhoamiShouldReturnTheUsernameFromGetUser(t *testing.T) {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v4/user": reply(200, `{"id":1,"username":"delamain-bot","name":"Delamain"}`),
	}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	login, err := Whoami(context.Background(), Repo{GitLab, "gitlab.com", "grp/api"}, "glpat-tok", WithBaseURL(srv.URL))
	if err != nil || login != "delamain-bot" {
		t.Fatalf("login = %q, err = %v", login, err)
	}
	if got := api.find(t, "GET", "/api/v4/user").header.Get("Private-Token"); got != "glpat-tok" {
		t.Fatalf("GET /user must carry the token, got %q", got)
	}
}

func TestGitLabPRViewShouldUseDiffRefsAndDetectFork(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/merge_requests/5": reply(200, `{"iid":5,"web_url":"https://gitlab.com/grp/api/-/merge_requests/5","title":"feat: x",
			"description":"Closes #2","author":{"username":"ana"},"draft":true,"state":"opened","sha":"h1",
			"source_branch":"feat/x","target_branch":"main","source_project_id":10,"target_project_id":10,
			"diff_refs":{"base_sha":"b1","head_sha":"h1","start_sha":"s1"}}`),
		"GET " + glProject + "/merge_requests/6": reply(200, `{"iid":6,"state":"merged","source_branch":"patch-1","target_branch":"main",
			"source_project_id":20,"target_project_id":10,"diff_refs":{"base_sha":"b2","head_sha":"h2","start_sha":"s2"}}`),
	})
	ctx := context.Background()
	pr, err := f.PRView(ctx, 5)
	want := PRDetail{Number: 5, URL: "https://gitlab.com/grp/api/-/merge_requests/5", Title: "feat: x", Body: "Closes #2", Author: "ana",
		Draft: true, Open: true, BaseRef: "main", HeadRef: "feat/x", BaseSHA: "b1", HeadSHA: "h1", StartSHA: "s1"}
	if err != nil || pr != want {
		t.Fatalf("mr = %+v, err = %v; want %+v", pr, err, want)
	}

	pr, err = f.PRView(ctx, 6)
	if err != nil || !pr.Fork || pr.Open || pr.HeadRef != "patch-1" || pr.BaseSHA != "b2" || pr.StartSHA != "s2" {
		t.Fatalf("a source project other than the target is a fork: %+v %v", pr, err)
	}
}

func TestGitLabThreadsShouldSkipSystemNotes(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/merge_requests/12/discussions": reply(200, `[
			{"id":"d1","notes":[{"id":1,"system":true,"body":"added 1 commit","created_at":"2026-01-01T07:00:00Z"}]},
			{"id":"d2","notes":[
				{"id":2,"author":{"username":"ana"},"body":"fix this","created_at":"2026-01-01T10:00:00Z",
					"position":{"new_path":"a.go","new_line":10},"resolvable":true,"resolved":true},
				{"id":3,"author":{"username":"bob"},"body":"done","created_at":"2026-01-01T11:00:00Z","resolvable":true,"resolved":true}]},
			{"id":"d3","individual_note":true,"notes":[
				{"id":4,"author":{"username":"carol"},"body":"lgtm","created_at":"2026-01-01T09:00:00Z"}]},
			{"id":"d4","notes":[
				{"id":5,"system":true,"body":"changed line","created_at":"2026-01-01T06:00:00Z"},
				{"id":6,"author":{"username":"dan"},"body":"why?","created_at":"2026-01-01T08:00:00Z",
					"position":{"new_path":"c.go","new_line":3},"resolvable":true,"resolved":false}]}]`),
	})
	ts, err := f.Threads(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	want := []Thread{
		{ID: "d4", Path: "c.go", Line: 3, Comments: []Comment{{ID: "6", Author: "dan", Body: "why?", CreatedAt: at(t, "2026-01-01T08:00:00Z")}}},
		{ID: "d3", Comments: []Comment{{ID: "4", Author: "carol", Body: "lgtm", CreatedAt: at(t, "2026-01-01T09:00:00Z")}}},
		{ID: "d2", Path: "a.go", Line: 10, Resolved: true, Comments: []Comment{
			{ID: "2", Author: "ana", Body: "fix this", CreatedAt: at(t, "2026-01-01T10:00:00Z")},
			{ID: "3", Author: "bob", Body: "done", CreatedAt: at(t, "2026-01-01T11:00:00Z")},
		}},
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("threads =\n%+v\nwant, without system notes and by first comment,\n%+v", ts, want)
	}
	if q, _ := url.ParseQuery(api.find(t, "GET", glProject+"/merge_requests/12/discussions").query); q.Get("per_page") != "100" {
		t.Fatalf("must read up to 100 discussions: %v", q)
	}
}

func TestGitLabPostReviewShouldCreateOneDiscussionPerCommentWithPosition(t *testing.T) {
	discussions, notes := glProject+"/merge_requests/9/discussions", glProject+"/merge_requests/9/notes"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + discussions: reply(201, `{"id":"d1"}`),
		"POST " + notes:       reply(201, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "head1", BaseSHA: "base1", StartSHA: "start1", Summary: "summary",
		Comments: []InlineComment{{Path: "a.go", Line: 12, Body: "nil deref"}, {Path: "b/c.go", Line: 3, Body: "typo"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	for _, r := range api.requests() {
		if r.method == http.MethodPost && r.path == discussions {
			got = append(got, jsonBody(t, r))
		}
	}
	position := func(path string, line int) map[string]any {
		return map[string]any{
			"position_type": "text", "base_sha": "base1", "start_sha": "start1", "head_sha": "head1",
			"new_path": path, "old_path": path, "new_line": float64(line),
		}
	}
	want := []map[string]any{
		{"body": "nil deref", "position": position("a.go", 12)},
		{"body": "typo", "position": position("b/c.go", 3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discussions = %v\nwant %v", got, want)
	}
	if b := jsonBody(t, api.find(t, "POST", notes))["body"]; b != "summary" {
		t.Fatalf("summary note = %v", b)
	}
	if rs := api.requests(); rs[len(rs)-1].path != notes {
		t.Fatalf("the summary goes after the inline comments: %+v", rs)
	}
}

func TestGitLabPostReviewShouldMoveRejectedInlineCommentToSummary(t *testing.T) {
	discussions, notes := glProject+"/merge_requests/9/discussions", glProject+"/merge_requests/9/notes"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + discussions: sequence(reply(201, `{"id":"d1"}`), reply(400, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`)),
		"POST " + notes:       reply(201, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "h", BaseSHA: "b", StartSHA: "b", Summary: "summary",
		Comments: []InlineComment{{Path: "a.go", Line: 12, Body: "nil deref"}, {Path: "b/c.go", Line: 3, Body: "typo here"}},
	})
	if err != nil {
		t.Fatalf("an invalid position must not abort the review: %v", err)
	}
	if n := posts(api, discussions); n != 2 {
		t.Fatalf("discussions = %d, want 2 attempts", n)
	}
	summary, _ := jsonBody(t, api.find(t, "POST", notes))["body"].(string)
	if !strings.HasPrefix(summary, "summary") || !strings.Contains(summary, "b/c.go:3") || !strings.Contains(summary, "typo here") {
		t.Fatalf("the rejected comment must be appended to the summary: %q", summary)
	}
	if strings.Contains(summary, "nil deref") {
		t.Fatalf("an accepted comment must not be repeated in the summary: %q", summary)
	}
}

func TestGitLabPostReviewShouldFailWhenADiscussionFailsWithAnotherStatus(t *testing.T) {
	discussions, notes := glProject+"/merge_requests/9/discussions", glProject+"/merge_requests/9/notes"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + discussions: reply(403, `{"message":"403 Forbidden"}`),
		"POST " + notes:       reply(201, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "h", BaseSHA: "b", StartSHA: "b", Summary: "summary",
		Comments: []InlineComment{{Path: "a.go", Line: 12, Body: "nil deref"}},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("a non-400 failure must abort with its status: %v", err)
	}
	if n := posts(api, notes); n != 0 {
		t.Fatalf("an aborted review must not post the summary, got %d notes", n)
	}
}

func TestGitLabResolveThreadShouldMarkTheDiscussionResolved(t *testing.T) {
	path := glProject + "/merge_requests/9/discussions/d1"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"PUT " + path: reply(200, `{"id":"d1"}`),
	})
	if err := f.ResolveThread(context.Background(), 9, "d1"); err != nil {
		t.Fatal(err)
	}
	if b := jsonBody(t, api.find(t, "PUT", path)); b["resolved"] != true {
		t.Fatalf("body = %v, want resolved=true", b)
	}
}

func TestGitLabPostReviewShouldNotPostAnEmptySummary(t *testing.T) {
	discussions, notes := glProject+"/merge_requests/9/discussions", glProject+"/merge_requests/9/notes"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + discussions: reply(201, `{"id":"d1"}`),
		"POST " + notes:       reply(201, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "h", BaseSHA: "b", StartSHA: "b",
		Comments: []InlineComment{{Path: "a.go", Line: 12, Body: "nil deref"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := posts(api, discussions); n != 1 {
		t.Fatalf("discussions = %d, want 1", n)
	}
	if n := posts(api, notes); n != 0 {
		t.Fatalf("no summary and no rejected comment: want no note, got %d", n)
	}
}

func TestGitLabPostReviewShouldPostOnlyTheMovedCommentWhenTheSummaryIsEmpty(t *testing.T) {
	discussions, notes := glProject+"/merge_requests/9/discussions", glProject+"/merge_requests/9/notes"
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST " + discussions: reply(400, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`),
		"POST " + notes:       reply(201, `{"id":1}`),
	})
	err := f.PostReview(context.Background(), 9, ReviewOptions{
		HeadSHA: "h", BaseSHA: "b", StartSHA: "b",
		Comments: []InlineComment{{Path: "b/c.go", Line: 3, Body: "typo here"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if b := jsonBody(t, api.find(t, "POST", notes))["body"]; b != "`b/c.go:3`\n\ntypo here" {
		t.Fatalf("summary = %q, want just the moved comment", b)
	}
}

func TestGitLabShouldMarkConfidentialIssuesAndInternalNotes(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/8": reply(200, `{"iid":8,"title":"leak","description":"customer data","state":"opened","confidential":true}`),
		"GET " + glProject + "/issues/7": reply(200, `{"iid":7,"title":"public","state":"opened"}`),
		"GET " + glProject + "/merge_requests/12/discussions": reply(200, `[
			{"id":"d1","notes":[
				{"id":1,"author":{"username":"ana"},"body":"public note","created_at":"2026-01-01T10:00:00Z"},
				{"id":2,"author":{"username":"bob"},"body":"internal note","created_at":"2026-01-01T11:00:00Z","internal":true},
				{"id":3,"author":{"username":"eve"},"body":"old confidential note","created_at":"2026-01-01T12:00:00Z","confidential":true}]}]`),
	})
	ctx := context.Background()
	if is, err := f.Issue(ctx, 8); err != nil || !is.Confidential {
		t.Fatalf("a confidential issue must say so: %+v %v", is, err)
	}
	if is, err := f.Issue(ctx, 7); err != nil || is.Confidential {
		t.Fatalf("a public issue is not confidential: %+v %v", is, err)
	}
	ts, err := f.Threads(ctx, 12)
	if err != nil || len(ts) != 1 || len(ts[0].Comments) != 3 {
		t.Fatalf("threads: %+v %v", ts, err)
	}
	if c := ts[0].Comments; c[0].Internal || !c[1].Internal || !c[2].Internal {
		t.Fatalf("internal notes (and the deprecated confidential flag) must be marked: %+v", c)
	}
}

func TestGitLabWithOAuthShouldSendABearerToken(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7": reply(200, `{"iid":7,"title":"add x","state":"opened"}`),
	}, WithOAuth(nil))
	if _, err := f.Issue(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	h := api.find(t, "GET", glProject+"/issues/7").header
	if got := h.Get("Authorization"); got != "Bearer glpat-tok" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.Get("Private-Token"); got != "" {
		t.Fatalf("an OAuth token must not go as Private-Token: %q", got)
	}
}

type renewedToken []string

func (r *renewedToken) Token(context.Context) (string, error) {
	t := (*r)[0]
	if len(*r) > 1 {
		*r = (*r)[1:]
	}
	return t, nil
}

func TestGitLabWithOAuthShouldSendTheRenewedToken(t *testing.T) {
	renew := &renewedToken{"oauth-1", "oauth-2"}
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7": reply(200, `{"iid":7,"title":"add x","state":"opened"}`),
	}, WithOAuth(renew))
	for range 2 {
		if _, err := f.Issue(context.Background(), 7); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, c := range api.reqs {
		got = append(got, c.header.Get("Authorization"))
	}
	if strings.Join(got, ",") != "Bearer oauth-1,Bearer oauth-2" {
		t.Fatalf("Authorization of each request = %v", got)
	}
}

func TestGitLabThreadsShouldFollowPaginationPastOneHundredDiscussions(t *testing.T) {
	// A truncated list breaks the posted-comment dedupe and hides pending threads, so every
	// page has to be read.
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/merge_requests/12/discussions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				io.WriteString(w, `[{"id":"d2","notes":[{"id":2,"author":{"username":"bob"},"body":"segunda página","created_at":"2026-01-01T09:00:00Z"}]}]`)
				return
			}
			w.Header().Set("X-Next-Page", "2")
			io.WriteString(w, `[{"id":"d1","notes":[{"id":1,"author":{"username":"ana"},"body":"primeira página","created_at":"2026-01-01T08:00:00Z"}]}]`)
		},
	})
	ts, err := f.Threads(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].ID != "d1" || ts[1].ID != "d2" {
		t.Fatalf("both pages' discussions must come back: %+v", ts)
	}
	var calls int
	for _, rq := range api.requests() {
		if rq.method == http.MethodGet && strings.HasSuffix(rq.path, "/discussions") {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("the second page must be fetched exactly once, got %d calls", calls)
	}
}

func TestGitLabBlockersShouldKeepOnlyIsBlockedByLinks(t *testing.T) {
	f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7/links": reply(200, `[
			{"iid":3,"state":"opened","link_type":"is_blocked_by","references":{"full":"grp/api#3"}},
			{"iid":4,"state":"opened","link_type":"relates_to","references":{"full":"grp/api#4"}},
			{"iid":5,"state":"opened","link_type":"blocks","references":{"full":"grp/api#5"}},
			{"iid":6,"state":"opened","link_type":"is_blocked_by","references":{"full":"grp/api#6"}}]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "#3", Open: true}, {Number: 6, Ref: "#6", Open: true}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("blockers = %+v, want %+v", bs, want)
	}
	api.find(t, http.MethodGet, glProject+"/issues/7/links")
}

func TestGitLabBlockersShouldMapOpenedStateToOpen(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7/links": reply(200, `[
			{"iid":3,"state":"opened","link_type":"is_blocked_by","references":{"full":"grp/api#3"}},
			{"iid":5,"state":"closed","link_type":"is_blocked_by","references":{"full":"grp/api#5"}}]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "#3", Open: true}, {Number: 5, Ref: "#5", Open: false}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("blockers = %+v, want %+v", bs, want)
	}
}

func TestGitLabBlockersShouldNameBlockersFromAnotherProject(t *testing.T) {
	// #3 of evil/api must not be mistaken for #3 of grp/api.
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7/links": reply(200, `[
			{"iid":3,"state":"opened","link_type":"is_blocked_by","references":{"full":"evil/api#3"}},
			{"iid":3,"state":"opened","link_type":"is_blocked_by","references":{"full":"grp/sub/api#3"}},
			{"iid":4,"state":"closed","link_type":"is_blocked_by","references":{"full":"GRP/API#4"}}]`),
	})
	bs, err := f.Blockers(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []Blocker{{Number: 3, Ref: "evil/api#3", Open: true}, {Number: 3, Ref: "grp/sub/api#3", Open: true}, {Number: 4, Ref: "#4", Open: false}}
	if !reflect.DeepEqual(bs, want) {
		t.Fatalf("blockers = %+v, want %+v", bs, want)
	}
}

func TestGitLabBlockersShouldReturnEmptyWithoutBlockingLinks(t *testing.T) {
	// GitLab Free has no blocking links: the issue has none or only relates_to ones.
	for _, tc := range []struct{ name, links string }{
		{"no links", `[]`},
		{"only relates_to", `[{"iid":4,"state":"opened","link_type":"relates_to","references":{"full":"grp/api#4"}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, api := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
				"GET " + glProject + "/issues/7/links": reply(200, tc.links),
			})
			bs, err := f.Blockers(context.Background(), 7)
			if err != nil || len(bs) != 0 {
				t.Fatalf("an issue without blocking links must have no blockers: %+v %v", bs, err)
			}
			api.find(t, http.MethodGet, glProject+"/issues/7/links")
		})
	}
}

func TestGitLabBlockersShouldPropagateAPIErrors(t *testing.T) {
	f, _ := gitlabAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET " + glProject + "/issues/7/links": forbidden,
	})
	bs, err := f.Blockers(context.Background(), 7)
	wantForbidden(t, bs, err)
}
