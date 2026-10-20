package forge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func githubClientAt(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) (*githubClient, *fakeAPI) {
	t.Helper()
	f, api := githubAt(t, routes)
	return f.(*githubClient), api
}

func TestGraphQLShouldUseApiGraphQLOnGitHubEnterprise(t *testing.T) {
	api := &fakeAPI{routes: map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/graphql": reply(200, `{"data":{"viewer":{"login":"bot"}}}`),
	}}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	f, err := New(Repo{GitHub, strings.TrimPrefix(srv.URL, "https://"), "team/api"}, "tok", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Viewer struct{ Login string } }
	if err := f.(*githubClient).graphql(context.Background(), `query{viewer{login}}`, nil, &out); err != nil || out.Viewer.Login != "bot" {
		t.Fatalf("enterprise GraphQL lives at https://<host>/api/graphql, not /api/v3/graphql: %v %+v (requests %+v)", err, out, api.requests())
	}
}

func TestGraphQLShouldPostQueryAndDecodeData(t *testing.T) {
	g, api := githubClientAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":{"repository":{"issue":{"title":"add x"}}}}`),
	})
	var out struct {
		Repository struct {
			Issue struct{ Title string }
		}
	}
	err := g.graphql(context.Background(), `query($n:Int!){repository(owner:"acme",name:"api"){issue(number:$n){title}}}`, map[string]any{"n": 7}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Repository.Issue.Title != "add x" {
		t.Fatalf("out = %+v", out)
	}
	var sent struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	r := api.find(t, "POST", "/graphql")
	if err := json.Unmarshal([]byte(r.body), &sent); err != nil || !strings.Contains(sent.Query, "issue(number:$n)") || sent.Variables["n"] != float64(7) {
		t.Fatalf("request body = %s (%v)", r.body, err)
	}
	if r.header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("graphql must be authenticated: %v", r.header)
	}
}

func TestGraphQLShouldReturnErrorsArrayAsError(t *testing.T) {
	g, _ := githubClientAt(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /graphql": reply(200, `{"data":null,"errors":[{"message":"Could not resolve to a Repository"},{"message":"second"}]}`),
	})
	var out struct{}
	err := g.graphql(context.Background(), `query{viewer{login}}`, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a Repository") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("errors array must become the error, got %v", err)
	}
}
