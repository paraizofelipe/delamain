package decide

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

const testKey = "sk-or-v1-test-secret-key-0123456789"

// received is one call the fake Decisions API got.
type received struct {
	method, path string
	header       http.Header
	body         []byte
}

// fakeAPI is an httptest Decisions API that records every call and answers with reply.
type fakeAPI struct {
	mu    sync.Mutex
	calls []received
	reply func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, received{r.Method, r.URL.Path, r.Header.Clone(), b})
	f.mu.Unlock()
	f.reply(w, r)
}

func (f *fakeAPI) received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]received(nil), f.calls...)
}

// respond answers every call with status and body.
func respond(status int, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// serve starts a TLS fake API and a Client pointed at it.
func serve(t *testing.T, reply func(w http.ResponseWriter, r *http.Request), opts Options) (*Client, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{reply: reply}
	srv := httptest.NewTLSServer(api)
	t.Cleanup(srv.Close)
	opts.BaseURL = srv.URL + "/api/"
	opts.HTTP = srv.Client()
	if opts.APIKey == "" {
		opts.APIKey = testKey
	}
	if opts.Model == "" {
		opts.Model = "typesafe/jev-1.14"
	}
	return New(opts), api
}

// bugTriage asks the two questions realResponse answers.
func bugTriage() Request {
	return Request{
		State: map[string]any{"title": "Checkout fails", "body": "500 on pay"},
		Nouls: map[string]Noul{
			"is_bug": {Instructions: "Is this a bug report?", True: "describes a defect", False: "asks for a feature"},
		},
		Choices: map[string]Choice{
			"team": {Instructions: "Which team owns it?", Options: map[string]string{
				"payments": "billing and checkout", "frontend": "UI", "account": "login and profile",
			}},
		},
	}
}

func TestDecideShouldPostContractRequestWithBearerAndModel(t *testing.T) {
	c, api := serve(t, respond(200, realResponse), Options{})
	if _, err := c.Decide(context.Background(), bugTriage()); err != nil {
		t.Fatal(err)
	}
	calls := api.received()
	if len(calls) != 1 {
		t.Fatalf("requests = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.method != http.MethodPost || got.path != "/api/alpha/decisions" {
		t.Fatalf("request = %s %s, want POST /api/alpha/decisions", got.method, got.path)
	}
	if h := got.header.Get("Authorization"); h != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", h)
	}
	if h := got.header.Get("Content-Type"); h != "application/json" {
		t.Fatalf("Content-Type = %q", h)
	}
	var body any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, got.body)
	}
	want := map[string]any{
		"model": "typesafe/jev-1.14",
		"state": map[string]any{"title": "Checkout fails", "body": "500 on pay"},
		"questions": map[string]any{
			"is_bug": map[string]any{
				"type": "noul", "instructions": "Is this a bug report?",
				"criteria": map[string]any{"true": "describes a defect", "false": "asks for a feature"},
			},
			"team": map[string]any{
				"type": "choice", "instructions": "Which team owns it?",
				"criteria": map[string]any{"payments": "billing and checkout", "frontend": "UI", "account": "login and profile"},
			},
		},
	}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %s, want the Decisions API contract %v", got.body, want)
	}
}

func TestDecideShouldParseNoulAndChoiceAnswersFromRealResponse(t *testing.T) {
	c, _ := serve(t, respond(200, realResponse), Options{})
	res, err := c.Decide(context.Background(), bugTriage())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Answer{
		"is_bug": {Noul: 0.96},
		"team": {Choice: "payments", Confidence: 0.67, Probabilities: map[string]float64{
			"payments": 0.78, "frontend": 0.22, "account": 0,
		}},
	}
	if !reflect.DeepEqual(res.Answers, want) {
		t.Fatalf("answers = %+v, want %+v", res.Answers, want)
	}
}

func TestDecideShouldReportCostModelAndLatency(t *testing.T) {
	const delay = 30 * time.Millisecond
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		respond(200, realResponse)(w, r)
	}, Options{})
	res, err := c.Decide(context.Background(), bugTriage())
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != "typesafe/jev-1.13-20260917" {
		t.Fatalf("Model = %q, want the snapshot that answered", res.Model)
	}
	if res.CostUSD != 0.000019992 {
		t.Fatalf("CostUSD = %v, want usage.cost 0.000019992", res.CostUSD)
	}
	if res.Latency < delay {
		t.Fatalf("Latency = %v, want at least the %v the server took", res.Latency, delay)
	}
}
