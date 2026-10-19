package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// githubAt is a github.com client whose API lives on srv.
func githubAt(t *testing.T, srv *httptest.Server, opts ...Option) Forge {
	t.Helper()
	f, err := New(Repo{GitHub, "github.com", "acme/api"}, "ghp-test", append([]Option{WithBaseURL(srv.URL + "/")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGitHubShouldReadIssue(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/api/issues/7" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, `{"number":7,"title":"Fix add","body":"## Critérios","html_url":"https://github.com/acme/api/issues/7","state":"open","labels":[{"name":"bug"},{"name":"delamain:ready"}]}`)
	})
	is, err := githubAt(t, srv).Issue(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if is.Number != 7 || is.Title != "Fix add" || is.Body != "## Critérios" || is.URL != "https://github.com/acme/api/issues/7" ||
		!slices.Equal(is.Labels, []string{"bug", "delamain:ready"}) || !is.Open {
		t.Fatalf("issue = %+v", is)
	}
}

func TestGitHubShouldReadClosedIssueAsNotOpen(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"number":8,"title":"done","state":"closed","labels":[]}`)
	})
	is, err := githubAt(t, srv).Issue(context.Background(), 8)
	if err != nil || is.Number != 8 || is.Open {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
}

func TestGitHubShouldSendTokenAndUserAgent(t *testing.T) {
	var auth, ua string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth, ua = r.Header.Get("Authorization"), r.Header.Get("User-Agent")
		writeJSON(w, `{"number":1,"state":"open"}`)
	})
	if _, err := githubAt(t, srv, WithUserAgent("delamain/v1.2.3")).Issue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer ghp-test" || ua != "delamain/v1.2.3" {
		t.Fatalf("Authorization=%q User-Agent=%q", auth, ua)
	}
	if _, err := githubAt(t, srv).Issue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if ua != "delamain/dev" {
		t.Fatalf("default User-Agent = %q", ua)
	}
}

// Enterprise derives https://<host>/api/v3/ from the host itself, so no base URL override here.
func TestGitHubEnterpriseShouldUseApiV3BaseURL(t *testing.T) {
	var path string
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(w, `{"number":3,"state":"open"}`)
	})
	f, err := New(Repo{GitHub, srv.Listener.Addr().String(), "team/api"}, "ghe-test", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if is, err := f.Issue(context.Background(), 3); err != nil || is.Number != 3 {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
	if path != "/api/v3/repos/team/api/issues/3" {
		t.Fatalf("path = %q", path)
	}
}

func TestGitHubListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeJSON(w, `[{"number":9,"title":"b","body":"x","html_url":"u9","state":"open","labels":[{"name":"delamain:ready"}]},
			{"number":4,"title":"a","body":"y","html_url":"u4","state":"open","labels":[{"name":"delamain:ready"}]}]`)
	})
	got, err := githubAt(t, srv).ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 4 || got[0].Body != "y" || got[0].URL != "u4" || got[1].Number != 9 || !got[0].Open || got[1].Labels[0] != "delamain:ready" {
		t.Fatalf("FIFO by issue number, got %+v", got)
	}
	reqs := rc.find(http.MethodGet, "/repos/acme/api/issues")
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", rc.reqs)
	}
	q := reqs[0].Query
	for k, v := range map[string]string{"state": "open", "labels": "delamain:ready", "per_page": "100", "sort": "created", "direction": "asc"} {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q (%v)", k, q.Get(k), v, q)
		}
	}
}

// The issues endpoint also lists PRs; gh issue list never returned them.
func TestGitHubListReadyShouldSkipPullRequests(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `[{"number":5,"state":"open","pull_request":{"url":"https://api.github.com/repos/acme/api/pulls/5"}},{"number":6,"state":"open"}]`)
	})
	got, err := githubAt(t, srv).ListReady(context.Background(), "delamain:ready")
	if err != nil || len(got) != 1 || got[0].Number != 6 {
		t.Fatalf("got %+v, err = %v", got, err)
	}
}

func TestGitHubListReadyShouldReturnEmptyWhenNoIssueHasTheLabel(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, `[]`) })
	got, err := githubAt(t, srv).ListReady(context.Background(), "delamain:ready")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, err = %v", got, err)
	}
}

func TestGitHubOpenPRShouldReturnNilWhenNoneIsOpen(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeJSON(w, `[]`)
	})
	pr, err := githubAt(t, srv).OpenPR(context.Background(), "feat/x")
	if err != nil || pr != nil {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	reqs := rc.find(http.MethodGet, "/repos/acme/api/pulls")
	if len(reqs) != 1 || reqs[0].Query.Get("head") != "acme:feat/x" || reqs[0].Query.Get("state") != "open" {
		t.Fatalf("must filter open PRs by owner:branch: %+v", rc.reqs)
	}
}

func TestGitHubOpenPRShouldReturnTheExistingPR(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `[{"number":9,"html_url":"https://github.com/acme/api/pull/9","draft":true}]`)
	})
	pr, err := githubAt(t, srv).OpenPR(context.Background(), "feat/x")
	if err != nil || pr == nil || *pr != (PR{Number: 9, URL: "https://github.com/acme/api/pull/9", Draft: true}) {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
}

func TestGitHubShouldCreateDraftPR(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"number":42,"html_url":"https://github.com/acme/api/pull/42","draft":true}`)
	})
	pr, err := githubAt(t, srv).CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil || pr != (PR{Number: 42, URL: "https://github.com/acme/api/pull/42", Draft: true}) {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	reqs := rc.find(http.MethodPost, "/repos/acme/api/pulls")
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", rc.reqs)
	}
	for k, v := range map[string]any{"head": "fix/7-add", "base": "main", "title": "fix: add", "body": "Closes #7", "draft": true} {
		if reqs[0].Body[k] != v {
			t.Errorf("payload %s = %v, want %v", k, reqs[0].Body[k], v)
		}
	}
}

func TestGitHubShouldOpenReadyPRWhenNotDraft(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"number":1,"html_url":"https://github.com/acme/api/pull/1","draft":false}`)
	})
	pr, err := githubAt(t, srv).CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "feat: x"})
	if err != nil || pr.Draft || pr.Number != 1 {
		t.Fatalf("pr = %+v, err = %v", pr, err)
	}
	if reqs := rc.find(http.MethodPost, "/repos/acme/api/pulls"); len(reqs) != 1 || reqs[0].Body["draft"] == true {
		t.Fatalf("approved work must not be opened as draft: %+v", rc.reqs)
	}
}

func TestGitHubCommentShouldPostBodyAsJSON(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"id":1}`)
	})
	if err := githubAt(t, srv).Comment(context.Background(), 7, "olá & <b>"); err != nil {
		t.Fatal(err)
	}
	reqs := rc.find(http.MethodPost, "/repos/acme/api/issues/7/comments")
	if len(reqs) != 1 || reqs[0].Body["body"] != "olá & <b>" || !strings.HasPrefix(reqs[0].ContentType, "application/json") || len(reqs[0].Query) != 0 {
		t.Fatalf("comment requests = %+v", rc.reqs)
	}
}

const ghAlreadyExists = `{"message":"Validation Failed","errors":[{"resource":"Label","code":"already_exists","field":"name"}]}`

func TestGitHubShouldEnsureLabelsExistBeforeRelabeling(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		req := rc.add(r)
		switch {
		case req.Method == http.MethodPost && req.Path == "/repos/acme/api/labels" && req.Body["name"] == "delamain:ready":
			writeStatus(w, http.StatusUnprocessableEntity, ghAlreadyExists)
		case req.Method == http.MethodPost && req.Path == "/repos/acme/api/labels":
			writeStatus(w, http.StatusCreated, `{"name":"x"}`)
		default:
			writeJSON(w, `[]`)
		}
	})
	err := githubAt(t, srv).Relabel(context.Background(), 7, []string{"delamain:in-progress"}, []string{"delamain:ready"})
	if err != nil {
		t.Fatalf("an existing label is not an error: %v", err)
	}
	created := rc.find(http.MethodPost, "/repos/acme/api/labels")
	if len(created) != 2 {
		t.Fatalf("labels ensured = %+v, want 2 (added and removed)", created)
	}
	for _, c := range created {
		if c.Body["color"] != "5319e7" || c.Body["description"] != "delamain" {
			t.Errorf("label payload = %v", c.Body)
		}
	}
	added := rc.find(http.MethodPost, "/repos/acme/api/issues/7/labels")
	if len(added) != 1 || len(added[0].List) != 1 || added[0].List[0] != "delamain:in-progress" {
		t.Fatalf("add = %+v", added)
	}
	if removed := rc.find(http.MethodDelete, "/repos/acme/api/issues/7/labels/delamain:ready"); len(removed) != 1 {
		t.Fatalf("remove = %+v", rc.reqs)
	}
	last := rc.reqs[len(rc.reqs)-1]
	if last.Method != http.MethodDelete && last.Path != "/repos/acme/api/issues/7/labels" {
		t.Fatalf("labels must exist before they are applied: %+v", rc.reqs)
	}
}

func TestGitHubRelabelShouldPropagateRealLabelErrors(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
	})
	if err := githubAt(t, srv).Relabel(context.Background(), 7, []string{"x"}, nil); err == nil {
		t.Fatal("a permission error must not be swallowed")
	}
}

func TestGitHubRelabelShouldIgnoreRemovingALabelTheIssueDoesNotHave(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		req := rc.add(r)
		switch req.Method {
		case http.MethodDelete:
			writeStatus(w, http.StatusNotFound, `{"message":"Label does not exist"}`)
		default:
			writeStatus(w, http.StatusUnprocessableEntity, ghAlreadyExists)
		}
	})
	if err := githubAt(t, srv).Relabel(context.Background(), 7, nil, []string{"delamain:ready"}); err != nil {
		t.Fatalf("removing an absent label is a no-op: %v", err)
	}
	if len(rc.find(http.MethodPost, "/repos/acme/api/issues/7/labels")) != 0 {
		t.Fatalf("nothing to add, no add request: %+v", rc.reqs)
	}
	if len(rc.find(http.MethodDelete, "/repos/acme/api/issues/7/labels/delamain:ready")) != 1 {
		t.Fatalf("remove = %+v", rc.reqs)
	}
}
