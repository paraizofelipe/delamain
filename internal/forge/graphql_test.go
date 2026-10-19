package forge

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestGraphQLShouldPostQueryAndDecodeData(t *testing.T) {
	rc := &recorder{}
	var auth string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		rc.add(r)
		writeJSON(w, `{"data":{"repository":{"id":"R_1","issue":{"number":7}}}}`)
	})
	g := githubAt(t, srv).(github)
	var out struct {
		Repository struct {
			ID    string `json:"id"`
			Issue struct {
				Number int `json:"number"`
			} `json:"issue"`
		} `json:"repository"`
	}
	const q = `query($owner:String!,$n:Int!){repository(owner:$owner,name:"api"){id issue(number:$n){number}}}`
	if err := g.graphql(context.Background(), q, map[string]any{"owner": "acme", "n": 7}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Repository.ID != "R_1" || out.Repository.Issue.Number != 7 {
		t.Fatalf("out = %+v", out)
	}
	reqs := rc.find(http.MethodPost, "/graphql")
	if len(reqs) != 1 || auth != "Bearer ghp-test" || !strings.HasPrefix(reqs[0].ContentType, "application/json") {
		t.Fatalf("requests = %+v, auth = %q", rc.reqs, auth)
	}
	vars, _ := reqs[0].Body["variables"].(map[string]any)
	if reqs[0].Body["query"] != q || vars["owner"] != "acme" || vars["n"] != float64(7) {
		t.Fatalf("payload = %v", reqs[0].Body)
	}
}

func TestGraphQLShouldReturnErrorsArrayAsError(t *testing.T) {
	const secret = "ghp_GRAPHQLL3AK"
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository `+secret+`"},{"message":"second"}]}`)
	})
	f, err := New(Repo{GitHub, "github.com", "acme/api"}, secret, WithBaseURL(srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	err = f.(github).graphql(context.Background(), `query{viewer{login}}`, nil, &out)
	var apiErr *APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Op != "graphql" || !strings.Contains(err.Error(), "Could not resolve to a Repository") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestGraphQLShouldUseApiGraphQLPathOnEnterprise(t *testing.T) {
	var path string
	srv := serveTLS(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		writeJSON(w, `{"data":{}}`)
	})
	f, err := New(Repo{GitHub, srv.Listener.Addr().String(), "team/api"}, "ghe-test", WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := f.(github).graphql(context.Background(), `query{viewer{login}}`, nil, &out); err != nil {
		t.Fatal(err)
	}
	if path != "/api/graphql" {
		t.Fatalf("path = %q", path)
	}
}
