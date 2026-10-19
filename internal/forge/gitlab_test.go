package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// gitlabAt is a gitlab.com client whose API lives on srv.
func gitlabAt(t *testing.T, srv *httptest.Server, opts ...Option) Forge {
	t.Helper()
	f, err := New(Repo{GitLab, "gitlab.com", "grp/api"}, "glpat-test", append([]Option{WithBaseURL(srv.URL)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGitLabShouldReadIssueAndOpenMR(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/grp%2Fapi/issues/3":
			writeJSON(w, `{"iid":3,"title":"Add login","description":"- [ ] ok","web_url":"https://gitlab.com/grp/api/-/issues/3","state":"opened","labels":["delamain:ready"]}`)
		case "/api/v4/projects/grp%2Fapi/merge_requests":
			if r.URL.Query().Get("source_branch") != "feat/3-login" || r.URL.Query().Get("state") != "opened" {
				writeJSON(w, `[]`)
				return
			}
			writeJSON(w, `[{"iid":5,"web_url":"https://gitlab.com/grp/api/-/merge_requests/5","draft":false}]`)
		default:
			http.NotFound(w, r)
		}
	})
	f := gitlabAt(t, srv)
	is, err := f.Issue(context.Background(), 3)
	if err != nil || is.Number != 3 || is.Title != "Add login" || is.Body != "- [ ] ok" || is.URL != "https://gitlab.com/grp/api/-/issues/3" ||
		!is.Open || !slices.Equal(is.Labels, []string{"delamain:ready"}) {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
	pr, err := f.OpenPR(context.Background(), "feat/3-login")
	if err != nil || pr == nil || *pr != (PR{Number: 5, URL: "https://gitlab.com/grp/api/-/merge_requests/5"}) {
		t.Fatalf("open MR of the source branch: mr = %+v, err = %v", pr, err)
	}
}

func TestGitLabShouldReadClosedIssueAsNotOpen(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"iid":4,"title":"done","state":"closed","labels":[]}`)
	})
	is, err := gitlabAt(t, srv).Issue(context.Background(), 4)
	if err != nil || is.Number != 4 || is.Open {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
}

func TestGitLabShouldSendPrivateTokenAndUserAgent(t *testing.T) {
	var tok, ua string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		tok, ua = r.Header.Get("PRIVATE-TOKEN"), r.Header.Get("User-Agent")
		writeJSON(w, `{"iid":1,"state":"opened"}`)
	})
	if _, err := gitlabAt(t, srv, WithUserAgent("delamain/v1.2.3")).Issue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if tok != "glpat-test" || ua != "delamain/v1.2.3" {
		t.Fatalf("PRIVATE-TOKEN=%q User-Agent=%q", tok, ua)
	}
	if _, err := gitlabAt(t, srv).Issue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if ua != "delamain/dev" {
		t.Fatalf("default User-Agent = %q", ua)
	}
}

// Self-hosted derives https://<host>/api/v4 from the host itself, so no base URL override here.
func TestGitLabSelfHostedShouldUseApiV4BaseURL(t *testing.T) {
	var path string
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		writeJSON(w, `{"iid":5,"state":"opened"}`)
	})
	f, err := New(Repo{GitLab, srv.Listener.Addr().String(), "team/api"}, "glpat-test", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	if is, err := f.Issue(context.Background(), 5); err != nil || is.Number != 5 {
		t.Fatalf("issue = %+v, err = %v", is, err)
	}
	if path != "/api/v4/projects/team%2Fapi/issues/5" {
		t.Fatalf("path = %q", path)
	}
}

func TestGitLabShouldEncodeNestedGroupProjectPath(t *testing.T) {
	var path string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.EscapedPath()
		writeJSON(w, `{"iid":2,"state":"opened"}`)
	})
	f, err := New(Repo{GitLab, "gitlab.com", "grp/sub/api"}, "glpat-test", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Issue(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v4/projects/grp%2Fsub%2Fapi/issues/2" {
		t.Fatalf("path = %q", path)
	}
}

func TestGitLabListReadyShouldReturnOpenLabeledIssuesOldestFirst(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeJSON(w, `[{"iid":12,"title":"b","description":"x","web_url":"u12","state":"opened","labels":["delamain:ready"]},
			{"iid":3,"title":"a","description":"y","web_url":"u3","state":"opened","labels":["delamain:ready"]}]`)
	})
	got, err := gitlabAt(t, srv).ListReady(context.Background(), "delamain:ready")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Number != 3 || got[0].Body != "y" || got[1].Number != 12 || !got[1].Open {
		t.Fatalf("got %+v", got)
	}
	reqs := rc.find(http.MethodGet, "/api/v4/projects/grp/api/issues")
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", rc.reqs)
	}
	q := reqs[0].Query
	for k, v := range map[string]string{"state": "opened", "labels": "delamain:ready", "order_by": "created_at", "sort": "asc", "per_page": "100"} {
		if q.Get(k) != v {
			t.Errorf("query %s = %q, want %q (%v)", k, q.Get(k), v, q)
		}
	}
}

func TestGitLabOpenPRShouldReturnNilWhenNoneIsOpen(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeJSON(w, `[]`)
	})
	pr, err := gitlabAt(t, srv).OpenPR(context.Background(), "feat/x")
	if err != nil || pr != nil {
		t.Fatalf("mr = %+v, err = %v", pr, err)
	}
	reqs := rc.find(http.MethodGet, "/api/v4/projects/grp/api/merge_requests")
	if len(reqs) != 1 || reqs[0].Query.Get("source_branch") != "feat/x" || reqs[0].Query.Get("state") != "opened" {
		t.Fatalf("must filter open MRs by source branch: %+v", rc.reqs)
	}
}

func TestGitLabShouldCreateDraftMR(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"iid":15,"web_url":"https://gitlab.com/grp/api/-/merge_requests/15","draft":true}`)
	})
	pr, err := gitlabAt(t, srv).CreatePR(context.Background(), PROptions{Head: "fix/7-add", Base: "main", Title: "fix: add", Body: "Closes #7", Draft: true})
	if err != nil || pr != (PR{Number: 15, URL: "https://gitlab.com/grp/api/-/merge_requests/15", Draft: true}) {
		t.Fatalf("mr = %+v, err = %v", pr, err)
	}
	reqs := rc.find(http.MethodPost, "/api/v4/projects/grp/api/merge_requests")
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", rc.reqs)
	}
	for k, v := range map[string]any{"source_branch": "fix/7-add", "target_branch": "main", "title": "Draft: fix: add", "description": "Closes #7"} {
		if reqs[0].Body[k] != v {
			t.Errorf("payload %s = %v, want %v", k, reqs[0].Body[k], v)
		}
	}
}

func TestGitLabShouldCreateReadyMRWhenNotDraft(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"iid":1,"web_url":"u1","draft":false}`)
	})
	pr, err := gitlabAt(t, srv).CreatePR(context.Background(), PROptions{Head: "feat/x", Base: "main", Title: "feat: x"})
	if err != nil || pr.Draft || pr.Number != 1 {
		t.Fatalf("mr = %+v, err = %v", pr, err)
	}
	if reqs := rc.find(http.MethodPost, "/api/v4/projects/grp/api/merge_requests"); len(reqs) != 1 || reqs[0].Body["title"] != "feat: x" {
		t.Fatalf("approved work must not be opened as draft: %+v", rc.reqs)
	}
}

func TestGitLabCommentShouldPostNoteBodyAsJSON(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeStatus(w, http.StatusCreated, `{"id":1}`)
	})
	if err := gitlabAt(t, srv).Comment(context.Background(), 3, "status & <b>"); err != nil {
		t.Fatal(err)
	}
	reqs := rc.find(http.MethodPost, "/api/v4/projects/grp/api/issues/3/notes")
	if len(reqs) != 1 || reqs[0].Body["body"] != "status & <b>" || !strings.HasPrefix(reqs[0].ContentType, "application/json") || len(reqs[0].Query) != 0 {
		t.Fatalf("the note body goes in the JSON payload, never in the URL: %+v", rc.reqs)
	}
}

func TestGitLabRelabelShouldOmitEmptyLabelLists(t *testing.T) {
	rc := &recorder{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		writeJSON(w, `{"iid":3,"state":"opened"}`)
	})
	f := gitlabAt(t, srv)
	if err := f.Relabel(context.Background(), 3, []string{"a", "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Relabel(context.Background(), 3, nil, []string{"c"}); err != nil {
		t.Fatal(err)
	}
	reqs := rc.find(http.MethodPut, "/api/v4/projects/grp/api/issues/3")
	if len(reqs) != 2 {
		t.Fatalf("requests = %+v", rc.reqs)
	}
	if b := reqs[0].Body; b["add_labels"] != "a,b" || hasKey(b, "remove_labels") {
		t.Errorf("add only: %v", b)
	}
	if b := reqs[1].Body; b["remove_labels"] != "c" || hasKey(b, "add_labels") {
		t.Errorf("remove only: %v", b)
	}
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}
