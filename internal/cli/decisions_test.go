package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/decide"
)

// fakeDecisionsKey is an obviously fake decisions key: it never reaches a real provider.
const fakeDecisionsKey = "fake-decisions-key-for-cli-tests" //nolint:gosec // a test value, not a secret

// envOf answers getenv from vars only, so a test never depends on the developer's environment.
func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// decisionsCall is one request the fake Decisions API received.
type decisionsCall struct {
	path, auth string
	body       []byte
}

// fakeDecisions is an httptest Decisions API that records every call and answers with status and body.
type fakeDecisions struct {
	mu     sync.Mutex
	calls  []decisionsCall
	status int
	body   string
}

func (f *fakeDecisions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, decisionsCall{r.URL.Path, r.Header.Get("Authorization"), b})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.status)
	_, _ = io.WriteString(w, f.body)
}

func (f *fakeDecisions) received() []decisionsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]decisionsCall(nil), f.calls...)
}

// okAnswer is a 200 answer of the Decisions API to a single noul named ok.
const okAnswer = `{"model":"typesafe/jev-1.14-20261101","answers":{"ok":{"type":"noul","noul":0.9}},"usage":{"cost":0.00001}}`

// serveDecisions starts a fake Decisions API and an enabled decisions block pointed at it.
func serveDecisions(t *testing.T, status int, body string) (config.Decisions, *fakeDecisions) {
	t.Helper()
	api := &fakeDecisions{status: status, body: body}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	dc := config.Default().Decisions
	dc.Enabled = true
	dc.Provider.BaseURL = srv.URL + "/api"
	dc.Provider.Model = "typesafe/jev-test"
	dc.Provider.Timeout = 5 * time.Second
	return dc, api
}

func TestNewDeciderShouldReturnNilWhenDecisionsDisabled(t *testing.T) {
	dc := config.Default().Decisions
	d, err := newDecider(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey}))
	if err != nil {
		t.Fatalf("a disabled decisions block never fails: %v", err)
	}
	if d != nil {
		t.Fatalf("a disabled decisions block leaves the decider nil, got %T", d)
	}
}

func TestNewDeciderShouldFailWhenEnabledAndAPIKeyEnvIsEmpty(t *testing.T) {
	dc := config.Default().Decisions
	dc.Enabled = true
	d, err := newDecider(dc, envOf(nil))
	if err == nil {
		t.Fatalf("an enabled decisions block without a key must fail, got %T", d)
	}
	want := "decisions.enabled: variable DELAMAIN_DECISIONS_API_KEY is empty; set the key or turn decisions.enabled off"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	if d != nil {
		t.Fatalf("a failed decider is nil, got %T", d)
	}
}

func TestNewDeciderShouldBuildClientWithConfiguredProvider(t *testing.T) {
	dc, api := serveDecisions(t, http.StatusOK, okAnswer)
	d, err := newDecider(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.(*decide.Client); !ok {
		t.Fatalf("an enabled decisions block with a key builds the real client, got %T", d)
	}
	req := decide.Request{State: map[string]any{"text": "x"}, Nouls: map[string]decide.Noul{"ok": {Instructions: "?", True: "y", False: "n"}}}
	if _, err := d.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	calls := api.received()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(calls))
	}
	if calls[0].path != "/api/alpha/decisions" {
		t.Fatalf("path = %q, want the configured base_url + /alpha/decisions", calls[0].path)
	}
	if calls[0].auth != "Bearer "+fakeDecisionsKey {
		t.Fatalf("Authorization = %q, want the key read from api_key_env", calls[0].auth)
	}
	var body struct{ Model string }
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "typesafe/jev-test" {
		t.Fatalf("model = %q, want the configured model", body.Model)
	}
}

func TestNewDeciderShouldReadCustomAPIKeyEnvName(t *testing.T) {
	dc, api := serveDecisions(t, http.StatusOK, okAnswer)
	dc.Provider.APIKeyEnv = "MY_DECISIONS_KEY"
	_, err := newDecider(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey}))
	if err == nil || !strings.Contains(err.Error(), "MY_DECISIONS_KEY") {
		t.Fatalf("only the variable named by api_key_env holds the key; error = %v", err)
	}
	if strings.Contains(err.Error(), fakeDecisionsKey) {
		t.Fatalf("the error never carries a key value: %q", err)
	}
	d, err := newDecider(dc, envOf(map[string]string{"MY_DECISIONS_KEY": "custom-" + fakeDecisionsKey}))
	if err != nil {
		t.Fatal(err)
	}
	req := decide.Request{State: map[string]any{"text": "x"}, Nouls: map[string]decide.Noul{"ok": {Instructions: "?", True: "y", False: "n"}}}
	if _, err := d.Decide(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls := api.received(); len(calls) != 1 || calls[0].auth != "Bearer custom-"+fakeDecisionsKey {
		t.Fatalf("the client sends the key of the custom variable: %+v", calls)
	}
}

func TestDoctorDepsShouldSkipDecisionsProbeWhenDisabled(t *testing.T) {
	dc := config.Default().Decisions
	if probe := decisionsProbe(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey})); probe != nil {
		t.Fatal("a disabled decisions block skips the doctor probe")
	}
}

func TestDoctorDepsShouldSkipDecisionsProbeWhenAPIKeyEnvIsEmpty(t *testing.T) {
	dc, api := serveDecisions(t, http.StatusOK, okAnswer)
	if probe := decisionsProbe(dc, envOf(nil)); probe != nil {
		t.Fatal("without the key the doctor reports the missing variable and skips the probe")
	}
	if calls := api.received(); len(calls) != 0 {
		t.Fatalf("no request goes out without the key: %d", len(calls))
	}
}

func TestDoctorDepsShouldProbeDecisionsProviderWithMinimalRequest(t *testing.T) {
	dc, api := serveDecisions(t, http.StatusOK, okAnswer)
	probe := decisionsProbe(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey}))
	if probe == nil {
		t.Fatal("an enabled decisions block with a key probes the provider")
	}
	if err := probe(context.Background()); err != nil {
		t.Fatalf("a provider that answers passes the probe: %v", err)
	}
	calls := api.received()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want a single probe", len(calls))
	}
	var body struct {
		State     map[string]any `json:"state"`
		Questions map[string]struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(calls[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.State) != 1 || body.State["text"] != "delamain doctor probe" {
		t.Fatalf("state = %v, want only the probe text", body.State)
	}
	ok, found := body.Questions["ok"]
	if len(body.Questions) != 1 || !found || ok.Type != "noul" {
		t.Fatalf("questions = %+v, want a single noul named ok", body.Questions)
	}
	if ok.Instructions != "Is the text a connection test?" || ok.Criteria["true"] != "Yes" || ok.Criteria["false"] != "No" {
		t.Fatalf("noul ok = %+v", ok)
	}
}

func TestDoctorDepsShouldNotLeakAPIKeyWhenDecisionsProbeFails(t *testing.T) {
	dc, _ := serveDecisions(t, http.StatusUnauthorized, `{"error":{"message":"invalid key `+fakeDecisionsKey+`"}}`)
	probe := decisionsProbe(dc, envOf(map[string]string{config.DefaultDecisionsAPIKeyEnv: fakeDecisionsKey}))
	if probe == nil {
		t.Fatal("an enabled decisions block with a key probes the provider")
	}
	err := probe(context.Background())
	if err == nil {
		t.Fatal("a provider that refuses the key fails the probe")
	}
	if strings.Contains(err.Error(), fakeDecisionsKey) {
		t.Fatalf("the probe error never carries the key: %q", err)
	}
}
