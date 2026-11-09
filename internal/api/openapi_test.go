package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/control"
)

// doc is the OpenAPI document as the test reads it.
type doc struct {
	OpenAPI  string                    `yaml:"openapi"`
	Security []map[string][]string     `yaml:"security"`
	Paths    map[string]map[string]any `yaml:"paths"`
	Comp     map[string]map[string]any `yaml:"components"`
	Servers  []map[string]string       `yaml:"servers"`
	Info     map[string]any            `yaml:"info"`
	Tags     []map[string]string       `yaml:"tags"`
	Extra    map[string]any            `yaml:",inline"`
}

func parseDoc(t *testing.T) doc {
	t.Helper()
	var d doc
	if err := yaml.Unmarshal(OpenAPI(), &d); err != nil {
		t.Fatalf("openapi.yaml does not parse: %v", err)
	}
	return d
}

func TestOpenAPIShouldDescribeEveryRouteTheServerServes(t *testing.T) {
	d := parseDoc(t)
	if !strings.HasPrefix(d.OpenAPI, "3.1") {
		t.Fatalf("openapi = %q, want 3.1.x", d.OpenAPI)
	}
	srv := New(&fakeSource{}, control.New(control.Deps{}), nil, Options{Token: []byte(testToken)})
	for _, route := range srv.Routes() {
		method, path, _ := strings.Cut(route, " ")
		ops, ok := d.Paths[path]
		if !ok {
			t.Errorf("route %s has no paths entry for %s", route, path)
			continue
		}
		if _, ok := ops[strings.ToLower(method)]; !ok {
			t.Errorf("route %s has no %s operation under %s", route, strings.ToLower(method), path)
		}
	}
	for path, ops := range d.Paths {
		for method := range ops {
			if method == "parameters" {
				continue
			}
			route := strings.ToUpper(method) + " " + path
			if !slices.Contains(srv.Routes(), route) {
				t.Errorf("the document describes %s, which is not served", route)
			}
		}
	}
}

func TestOpenAPIShouldEnumerateEveryProblemCodeAndRequireTheBearer(t *testing.T) {
	d := parseDoc(t)
	schemas := d.Comp["schemas"]
	problem, _ := schemas["Problem"].(map[string]any)
	props, _ := problem["properties"].(map[string]any)
	code, _ := props["code"].(map[string]any)
	raw, _ := code["enum"].([]any)
	var listed []string
	for _, v := range raw {
		listed = append(listed, v.(string))
	}
	for _, c := range Codes {
		if !slices.Contains(listed, c) {
			t.Errorf("code %q is emitted but not listed in the Problem schema", c)
		}
	}
	for _, c := range listed {
		if !slices.Contains(Codes, c) {
			t.Errorf("code %q is listed but never emitted", c)
		}
	}
	if len(d.Security) != 1 || d.Security[0]["bearer"] == nil {
		t.Fatalf("the bearer is required globally: %v", d.Security)
	}
	schemes := d.Comp["securitySchemes"]
	bearer, _ := schemes["bearer"].(map[string]any)
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("securitySchemes.bearer = %v", bearer)
	}
	healthz := d.Paths["/healthz"]["get"].(map[string]any)
	if sec, ok := healthz["security"].([]any); !ok || len(sec) != 0 {
		t.Fatalf("/healthz is the one route without the bearer: %v", healthz["security"])
	}
	for path, ops := range d.Paths {
		for method, op := range ops {
			if method == "parameters" || path == "/healthz" {
				continue
			}
			if _, has := op.(map[string]any)["security"]; has {
				t.Errorf("%s %s overrides the global security", method, path)
			}
		}
	}
}

func TestOpenAPIIsServedBehindTheToken(t *testing.T) {
	ts := newTestServer(t, &fakeSource{})
	res := ts.get(t, "/api/v1/openapi.yaml")
	if res.Status != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/yaml") || !strings.HasPrefix(res.Body, "openapi: 3.1") {
		t.Fatalf("openapi: %d %q %q", res.Status, res.Header.Get("Content-Type"), res.Body[:min(40, len(res.Body))])
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/openapi.yaml", "", nil); res.Status != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", res.Status)
	}
}
