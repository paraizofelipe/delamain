package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "fake-decisions-key-for-tests"

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

func TestDecideShouldRejectRequestWithoutQuestions(t *testing.T) {
	c, api := serve(t, respond(200, realResponse), Options{})
	empty := map[string]Request{
		"nil maps":   {State: map[string]any{"title": "x"}},
		"empty maps": {State: map[string]any{"title": "x"}, Nouls: map[string]Noul{}, Choices: map[string]Choice{}},
	}
	for name, req := range empty {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Decide(context.Background(), req); !errors.Is(err, ErrNoQuestions) {
				t.Fatalf("err = %v, want ErrNoQuestions", err)
			}
		})
	}
	if n := len(api.received()); n != 0 {
		t.Fatalf("server got %d requests, want none", n)
	}
}

// stateOf returns a state whose JSON is exactly size bytes: {"body":"x...x"}.
func stateOf(t *testing.T, size int) map[string]any {
	t.Helper()
	s := map[string]any{"body": strings.Repeat("x", size-len(`{"body":""}`))}
	if b, _ := json.Marshal(s); len(b) != size {
		t.Fatalf("state is %d bytes, want %d", len(b), size)
	}
	return s
}

func TestDecideShouldRejectStateOver32000BytesBeforeSending(t *testing.T) {
	c, api := serve(t, respond(200, realResponse), Options{})
	req := bugTriage()
	req.State = stateOf(t, 32001)
	if _, err := c.Decide(context.Background(), req); !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("err = %v, want ErrStateTooLarge", err)
	}
	if n := len(api.received()); n != 0 {
		t.Fatalf("server got %d requests, want none", n)
	}

	req.State = stateOf(t, 32000)
	if _, err := c.Decide(context.Background(), req); err != nil {
		t.Fatalf("a state of exactly 32000 bytes is accepted: %v", err)
	}

	req.State = map[string]any{"ch": make(chan int)}
	if _, err := c.Decide(context.Background(), req); err == nil {
		t.Fatal("a state that is not JSON must fail")
	}
	if n := len(api.received()); n != 1 {
		t.Fatalf("server got %d requests, want only the 32000-byte one", n)
	}
}

func TestDecideShouldRejectQuestionNameUsedTwice(t *testing.T) {
	c, api := serve(t, respond(200, realResponse), Options{})
	req := bugTriage()
	req.Choices["is_bug"] = Choice{Instructions: "Kind?", Options: map[string]string{"bug": "a defect", "feature": "a request"}}
	_, err := c.Decide(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), `"is_bug"`) {
		t.Fatalf("err = %v, want a refusal naming the repeated question", err)
	}
	if n := len(api.received()); n != 0 {
		t.Fatalf("server got %d requests, want none", n)
	}
}

func TestDecideShouldReturnAPIErrorOnNon2xx(t *testing.T) {
	for _, status := range []int{400, 401, 413, 429, 500, 503, 524, 529} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			body := fmt.Sprintf(`{"error":{"code":%d,"message":"upstream said no"}}`, status)
			c, api := serve(t, respond(status, body+"\n"), Options{})
			_, err := c.Decide(context.Background(), bugTriage())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Status != status || apiErr.Message != body {
				t.Fatalf("APIError = %+v, want status %d and message %s", apiErr, status, body)
			}
			if want := fmt.Sprintf("decisions: %d %s", status, body); err.Error() != want {
				t.Fatalf("Error() = %q, want %q", err.Error(), want)
			}
			if n := len(api.received()); n != 1 {
				t.Fatalf("server got %d requests, want 1 (no retry)", n)
			}
		})
	}
}

func TestDecideShouldTruncateAPIErrorMessageTo300Bytes(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		// "a" then two-byte runes: byte 300 is mid-rune, so the cut falls back to byte 299.
		"never inside a UTF-8 sequence": {"a" + strings.Repeat("é", 300), "a" + strings.Repeat("é", 149)},
		// The key straddles byte 300: it is replaced before the cut, so no prefix of it survives.
		"key replaced before the cut": {strings.Repeat("x", 295) + testKey, strings.Repeat("x", 295) + "[reda"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, respond(500, tc.body), Options{})
			_, err := c.Decide(context.Background(), bugTriage())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v, want *APIError", err)
			}
			if apiErr.Message != tc.want {
				t.Fatalf("Message = %q (%d bytes), want %q", apiErr.Message, len(apiErr.Message), tc.want)
			}
			if strings.Contains(err.Error(), testKey[:5]) {
				t.Fatalf("error leaks a prefix of the key: %v", err)
			}
		})
	}
}

func TestDecideShouldNeverLeakAPIKeyInErrors(t *testing.T) {
	t.Run("error body echoes the Authorization header", func(t *testing.T) {
		c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"bad header %s"}}`, r.Header.Get("Authorization"))
		}, Options{})
		_, err := c.Decide(context.Background(), bugTriage())
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Fatalf("err = %v, want an error without the key", err)
		}
		if !strings.Contains(err.Error(), "bad header Bearer [redacted]") {
			t.Fatalf("err = %v, want the key replaced by [redacted]", err)
		}
	})
	t.Run("answer echoes the Authorization header", func(t *testing.T) {
		c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
			respond(200, strings.Replace(realResponse, `"choice": "payments"`, `"choice": "`+r.Header.Get("Authorization")+`"`, 1))(w, r)
		}, Options{})
		_, err := c.Decide(context.Background(), bugTriage())
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Fatalf("err = %v, want an error without the key", err)
		}
	})
	t.Run("request that never reaches the server", func(t *testing.T) {
		// A key pasted into the base URL makes the URL invalid; the parse error quotes the URL.
		c := New(Options{BaseURL: "https://openrouter.invalid/" + testKey + "\x7f", APIKey: testKey})
		_, err := c.Decide(context.Background(), bugTriage())
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Fatalf("err = %v, want an error without the key", err)
		}
	})
}

func TestDecideShouldFailOnTimeout(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			respond(200, realResponse)(w, r)
		}
	}, Options{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Decide(context.Background(), bugTriage())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Decide took %v, want it to give up after the 50ms Timeout", elapsed)
	}
}

func TestDecideShouldRejectMalformedJSONBody(t *testing.T) {
	c, _ := serve(t, respond(200, `{"answers": {"is_bug": `), Options{})
	_, err := c.Decide(context.Background(), bugTriage())
	if err == nil || !strings.HasPrefix(err.Error(), "decisions: ") {
		t.Fatalf("err = %v, want a decisions error", err)
	}
}

func TestDecideShouldRejectResponseBodyOver1MiB(t *testing.T) {
	// A valid answer, padded past 1 MiB: only the size limit can refuse it.
	big := strings.Replace(realResponse, `"provider"`, `"pad": "`+strings.Repeat("x", 1<<20)+`", "provider"`, 1)
	c, _ := serve(t, respond(200, big), Options{})
	_, err := c.Decide(context.Background(), bugTriage())
	if err == nil || !strings.HasPrefix(err.Error(), "decisions: ") {
		t.Fatalf("err = %v, want a decisions error", err)
	}
}

// answering serves realResponse with old replaced by new, failing if old is not in it.
func answering(t *testing.T, old, new string) func(w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if !strings.Contains(realResponse, old) {
		t.Fatalf("fixture has no %s", old)
	}
	return respond(200, strings.Replace(realResponse, old, new, 1))
}

// wantInvalid fails unless err is the invalid-answer error of question.
func wantInvalid(t *testing.T, err error, question string) {
	t.Helper()
	if prefix := "invalid decision answer: " + question + ": "; err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("err = %v, want %q...", err, prefix)
	}
}

func TestDecideShouldRejectNoulOutsideZeroToOne(t *testing.T) {
	for _, noul := range []string{"-0.01", "1.01"} {
		t.Run(noul, func(t *testing.T) {
			c, _ := serve(t, answering(t, `"noul": 0.96`, `"noul": `+noul), Options{})
			_, err := c.Decide(context.Background(), bugTriage())
			wantInvalid(t, err, "is_bug")
		})
	}
	for _, noul := range []string{"0", "1"} {
		t.Run(noul, func(t *testing.T) {
			c, _ := serve(t, answering(t, `"noul": 0.96`, `"noul": `+noul), Options{})
			if _, err := c.Decide(context.Background(), bugTriage()); err != nil {
				t.Fatalf("noul %s is inside [0,1]: %v", noul, err)
			}
		})
	}
}

func TestDecideShouldRejectChoiceNotAmongOptions(t *testing.T) {
	c, _ := serve(t, answering(t, `"choice": "payments"`, `"choice": "legal"`), Options{})
	_, err := c.Decide(context.Background(), bugTriage())
	wantInvalid(t, err, "team")
}

func TestDecideShouldRejectMissingAnswer(t *testing.T) {
	cases := map[string]struct{ old, new, question string }{
		"noul not answered":   {`"is_bug": { "type": "noul", "noul": 0.96 },`, ``, "is_bug"},
		"choice not answered": {`"team": { "type": "choice"`, `"other": { "type": "choice"`, "team"},
		"noul without value":  {`"type": "noul", "noul": 0.96`, `"type": "noul"`, "is_bug"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, answering(t, tc.old, tc.new), Options{})
			_, err := c.Decide(context.Background(), bugTriage())
			wantInvalid(t, err, tc.question)
		})
	}
}

func TestDecideShouldRejectAnswerOfWrongType(t *testing.T) {
	cases := map[string]struct{ old, new, question string }{
		"noul answered as choice": {`"type": "noul", "noul": 0.96`, `"type": "choice", "choice": "payments", "noul": 0.96`, "is_bug"},
		"choice answered as noul": {`"type": "choice", "choice": "payments"`, `"type": "noul", "noul": 0.5, "choice": "payments"`, "team"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := serve(t, answering(t, tc.old, tc.new), Options{})
			_, err := c.Decide(context.Background(), bugTriage())
			wantInvalid(t, err, tc.question)
		})
	}
}
