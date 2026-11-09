package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/control"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/store"
)

const testToken = "dlm_0123456789abcdefghijklmnopqrstuvwxyzABCDEF"

// clock is a time tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeSource is a Source over fixed data.
type fakeSource struct {
	mu     sync.Mutex
	runs   []board.Run
	queue  []board.QueueItem
	status board.Status
	repos  []Repo
	err    error
	calls  int
}

func (s *fakeSource) Runs(context.Context, int) ([]board.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return append([]board.Run(nil), s.runs...), s.err
}

func (s *fakeSource) Run(_ context.Context, id int64) (board.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return board.Run{}, board.ErrNotFound
}

func (s *fakeSource) Queue(context.Context) ([]board.QueueItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]board.QueueItem(nil), s.queue...), s.err
}

func (s *fakeSource) Status(context.Context) (board.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.err
}

func (s *fakeSource) Report(context.Context, board.ReportQuery) (board.Report, error) {
	return board.Report{Period: "7d"}, nil
}

func (s *fakeSource) Repos(context.Context) ([]Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Repo(nil), s.repos...), nil
}

// RunsBefore pages s.runs by id, newest first, as the store does.
func (s *fakeSource) RunsBefore(_ context.Context, before int64, limit int, filter string) ([]board.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	sorted := append([]board.Run(nil), s.runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID > sorted[j].ID })
	var out []board.Run
	for _, r := range sorted {
		terminal := r.State == "recorded" || r.State == "needs_info"
		if (before > 0 && r.ID >= before) || (filter == "active" && terminal) || (filter == "finished" && !terminal) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, r)
	}
	return out, nil
}

// newTestServer is a Server over src and acts, auditing to a temp file, with a hand-moved clock.
func newTestServer(t *testing.T, src *fakeSource, setup ...serverOption) *testServer {
	t.Helper()
	if src == nil {
		src = &fakeSource{}
	}
	clk := newClock()
	auditPath := filepath.Join(t.TempDir(), "state", "api-audit.log")
	audit, err := OpenAudit(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	opts := Options{Token: []byte(testToken), Now: clk.now, Interval: 20 * time.Millisecond, Version: "vtest"}
	for _, f := range setup {
		f(&opts)
	}
	srv := New(src, nil, audit, opts)
	ctx, cancel := context.WithCancel(context.Background())
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	go srv.Run(ctx)
	ts.Start()
	t.Cleanup(func() {
		cancel()
		ts.Close()
	})
	return &testServer{Server: ts, srv: srv, clock: clk, audit: auditPath}
}

// testServer is a Server under test, with its audit file and clock.
type testServer struct {
	*httptest.Server
	srv   *Server
	clock *clock
	audit string
}

type serverOption func(*Options)

// response is what a request answered, body read and closed.
type response struct {
	Status int
	Header http.Header
	Body   string
}

// json decodes the body into v.
func (r response) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(r.Body), v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.Body)
	}
}

// problem is the body as a problem+json.
func (r response) problem(t *testing.T) map[string]any {
	t.Helper()
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want problem+json:\n%s", ct, r.Body)
	}
	var p map[string]any
	r.json(t, &p)
	return p
}

// do sends method path with the token (or none when token is "") and headers.
func (ts *testServer) do(t *testing.T, method, path, token string, body io.Reader, headers ...string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return response{Status: res.StatusCode, Header: res.Header, Body: string(b)}
}

func (ts *testServer) get(t *testing.T, path string) response {
	t.Helper()
	return ts.do(t, http.MethodGet, path, testToken, nil)
}

// auditLines are the audit file's lines, parsed.
func (ts *testServer) auditLines(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(ts.audit)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("audit line is not JSON: %v\n%s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// pingRoutes are test routes behind the token: one that answers, one that panics, one that
// decodes a body, one mutation that notes a run.
func pingRoutes(srv *Server) {
	srv.testRoutes = map[string]http.HandlerFunc{
		"GET /api/v1/ping": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{"pong": "1"})
		},
		"GET /api/v1/panic":  func(http.ResponseWriter, *http.Request) { panic("boom") },
		"GET /api/v1/stream": func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, nil) },
		"POST /api/v1/echo": func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				A int `json:"a"`
			}
			if err := decodeJSON(w, r, &body); err != nil {
				fail(w, r, err)
				return
			}
			noteAudit(r, "run_id", int64(7))
			writeJSON(w, http.StatusAccepted, body)
		},
	}
}

// newPingServer is a test server with the ping routes.
func newPingServer(t *testing.T, setup ...serverOption) *testServer {
	t.Helper()
	clk := newClock()
	auditPath := filepath.Join(t.TempDir(), "state", "api-audit.log")
	audit, err := OpenAudit(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	opts := Options{Token: []byte(testToken), Now: clk.now, Interval: 20 * time.Millisecond}
	for _, f := range setup {
		f(&opts)
	}
	srv := New(&fakeSource{}, nil, audit, opts)
	pingRoutes(srv)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testServer{Server: ts, srv: srv, clock: clk, audit: auditPath}
}

func TestHealthzNeedsNoTokenAndEverythingElseDoes(t *testing.T) {
	ts := newPingServer(t)
	if res := ts.do(t, http.MethodGet, "/healthz", "", nil); res.Status != http.StatusOK || strings.TrimSpace(res.Body) != `{"ok":true}` {
		t.Fatalf("healthz: %d %q", res.Status, res.Body)
	}
	res := ts.do(t, http.MethodGet, "/api/v1/ping", "", nil)
	p := res.problem(t)
	if res.Status != http.StatusUnauthorized || p["code"] != codeUnauthorized || p["status"] != float64(401) || p["instance"] != "/api/v1/ping" {
		t.Fatalf("no token: %d %v", res.Status, p)
	}
	if res.Header.Get("WWW-Authenticate") != `Bearer realm="delamain"` {
		t.Fatalf("WWW-Authenticate = %q", res.Header.Get("WWW-Authenticate"))
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil); res.Status != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", res.Status)
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", testToken+"x", nil); res.Status != http.StatusUnauthorized {
		t.Fatalf("a longer token: %d", res.Status)
	}
	if res := ts.get(t, "/api/v1/ping"); res.Status != http.StatusOK || !strings.Contains(res.Body, "pong") {
		t.Fatalf("the token opens the API: %d %q", res.Status, res.Body)
	}
	lines := ts.auditLines(t)
	if len(lines) != 3 {
		t.Fatalf("every refused login is audited, nothing else: %v", lines)
	}
	l := lines[0]
	if l["event"] != eventAuthFailed || l["status"] != float64(401) || l["path"] != "/api/v1/ping" || l["method"] != "GET" || l["client_ip"] != "127.0.0.1" || l["error"] != codeUnauthorized {
		t.Fatalf("audit line = %v", l)
	}
	if _, ok := l["time"]; !ok {
		t.Fatalf("the line is dated: %v", l)
	}
	if strings.Contains(string(mustRead(t, ts.audit)), "wrong") || strings.Contains(string(mustRead(t, ts.audit)), testToken) {
		t.Fatal("the audit never holds a token, right or wrong")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	ts := newPingServer(t)
	for _, res := range []response{ts.do(t, http.MethodGet, "/healthz", "", nil), ts.get(t, "/api/v1/nope"), ts.do(t, http.MethodGet, "/api/v1/ping", "", nil)} {
		h := res.Header
		if h.Get("Content-Security-Policy") != "default-src 'none'; frame-ancestors 'none'" || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
			t.Fatalf("headers = %v", h)
		}
		if h.Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("no CORS: there is no browser client")
		}
	}
}

func TestUnknownRoutesAndMethodsAreProblems(t *testing.T) {
	ts := newPingServer(t)
	res := ts.get(t, "/api/v1/nope")
	if p := res.problem(t); res.Status != http.StatusNotFound || p["code"] != codeNotFound || p["title"] != "Not Found" || p["type"] != "about:blank" {
		t.Fatalf("unknown route: %d %v", res.Status, p)
	}
	res = ts.do(t, http.MethodPost, "/healthz", "", nil)
	if p := res.problem(t); res.Status != http.StatusMethodNotAllowed || p["code"] != codeMethodNotAllowed || res.Header.Get("Allow") != "GET" {
		t.Fatalf("wrong method: %d %v allow=%q", res.Status, p, res.Header.Get("Allow"))
	}
	res = ts.do(t, http.MethodDelete, "/api/v1/ping", testToken, nil)
	if res.Status != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET" {
		t.Fatalf("wrong method behind the token: %d allow=%q", res.Status, res.Header.Get("Allow"))
	}
	if res := ts.do(t, http.MethodGet, "/nope", "", nil); res.Status != http.StatusNotFound {
		t.Fatalf("outside the API: %d", res.Status)
	}
}

func TestTooManyFailedLoginsLockTheClientOutEvenWithTheRightToken(t *testing.T) {
	ts := newPingServer(t, func(o *Options) { o.AuthFailures = 3 })
	for i := range 3 {
		if res := ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil); i < 2 && res.Status != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, res.Status)
		} else if i == 2 && res.Status != http.StatusTooManyRequests {
			t.Fatalf("the third failure locks the client out: %d", res.Status)
		}
	}
	res := ts.get(t, "/api/v1/ping")
	if p := res.problem(t); res.Status != http.StatusTooManyRequests || p["code"] != codeRateLimited || res.Header.Get("Retry-After") != "900" {
		t.Fatalf("locked out, the right token is refused too, for 15 minutes: %d %v retry=%q", res.Status, p, res.Header.Get("Retry-After"))
	}
	ts.clock.advance(14 * time.Minute)
	if res := ts.get(t, "/api/v1/ping"); res.Status != http.StatusTooManyRequests || res.Header.Get("Retry-After") != "60" {
		t.Fatalf("still locked: %d retry=%q", res.Status, res.Header.Get("Retry-After"))
	}
	ts.clock.advance(time.Minute)
	if res := ts.get(t, "/api/v1/ping"); res.Status != http.StatusOK {
		t.Fatalf("the lockout ends: %d", res.Status)
	}
	lines := ts.auditLines(t)
	var events []string
	for _, l := range lines {
		events, _ = append(events, l["event"].(string)), l
	}
	if strings.Join(events, ",") != "auth_failed,auth_failed,rate_limited,rate_limited,rate_limited" {
		t.Fatalf("events = %v", events)
	}
}

func TestFailuresOlderThanTheWindowAreForgotten(t *testing.T) {
	ts := newPingServer(t, func(o *Options) { o.AuthFailures = 3 })
	ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil)
	ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil)
	ts.clock.advance(11 * time.Minute)
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil); res.Status != http.StatusUnauthorized {
		t.Fatalf("two old failures and a new one do not lock: %d", res.Status)
	}
	if res := ts.get(t, "/api/v1/ping"); res.Status != http.StatusOK {
		t.Fatalf("a good login after a failure clears nothing but passes: %d", res.Status)
	}
}

func TestTrustProxyKeysTheLockoutByTheForwardedClient(t *testing.T) {
	ts := newPingServer(t, func(o *Options) { o.AuthFailures = 2; o.TrustProxy = true })
	for range 2 {
		ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "CF-Connecting-IP", "203.0.113.5")
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "CF-Connecting-IP", "203.0.113.5"); res.Status != http.StatusTooManyRequests {
		t.Fatalf("the failing phone is locked out: %d", res.Status)
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "X-Forwarded-For", "198.51.100.7, 10.0.0.1"); res.Status != http.StatusUnauthorized {
		t.Fatalf("another client, by the first X-Forwarded-For, is not: %d", res.Status)
	}
	if res := ts.do(t, http.MethodGet, "/api/v1/ping", testToken, nil, "CF-Connecting-IP", "198.51.100.9"); res.Status != http.StatusOK {
		t.Fatalf("a third client logs in: %d", res.Status)
	}
	lines := ts.auditLines(t)
	if lines[0]["client_ip"] != "203.0.113.5" || lines[3]["client_ip"] != "198.51.100.7" {
		t.Fatalf("the audit names the forwarded client: %v", lines)
	}

	shared := newPingServer(t, func(o *Options) { o.AuthFailures = 2 })
	shared.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "CF-Connecting-IP", "203.0.113.5")
	shared.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "CF-Connecting-IP", "203.0.113.6")
	if res := shared.do(t, http.MethodGet, "/api/v1/ping", "wrong", nil, "CF-Connecting-IP", "203.0.113.7"); res.Status != http.StatusTooManyRequests {
		t.Fatalf("without trust_proxy the headers are ignored and every request is one client: %d", res.Status)
	}
	if shared.auditLines(t)[0]["client_ip"] != "127.0.0.1" {
		t.Fatal("without trust_proxy the audit names the peer")
	}
}

func TestClientIPShouldIgnoreTheHeadersOfANonLoopbackPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.1:4242"
	r.Header.Set("CF-Connecting-IP", "203.0.113.5")
	if got := clientIP(r, true); got != "198.51.100.1" {
		t.Fatalf("a direct peer cannot pick its own address: %q", got)
	}
	r.RemoteAddr = "[::1]:4242"
	if got := clientIP(r, true); got != "203.0.113.5" {
		t.Fatalf("a loopback peer is the tunnel: %q", got)
	}
	if got := clientIP(r, false); got != "::1" {
		t.Fatalf("not trusting the proxy: %q", got)
	}
}

func TestTheGlobalRateLimitRefusesABurst(t *testing.T) {
	ts := newPingServer(t, func(o *Options) { o.RequestsPerSecond = 2 })
	ok := 0
	var last response
	for range 6 {
		last = ts.do(t, http.MethodGet, "/healthz", "", nil)
		if last.Status == http.StatusOK {
			ok++
		}
	}
	if ok != 4 || last.Status != http.StatusTooManyRequests || last.Header.Get("Retry-After") != "1" {
		t.Fatalf("a burst of twice the rate passes, the rest is 429: ok=%d last=%d", ok, last.Status)
	}
	if p := last.problem(t); p["code"] != codeRateLimited {
		t.Fatalf("problem = %v", p)
	}
	ts.clock.advance(time.Second)
	if res := ts.do(t, http.MethodGet, "/healthz", "", nil); res.Status != http.StatusOK {
		t.Fatalf("tokens come back with time: %d", res.Status)
	}
	if lines := ts.auditLines(t); len(lines) != 2 || lines[0]["event"] != eventRateLimited || lines[0]["status"] != float64(429) {
		t.Fatalf("throttled requests are audited: %v", lines)
	}
}

func TestAPanicIsA500AndTheServerStaysUp(t *testing.T) {
	ts := newPingServer(t)
	res := ts.get(t, "/api/v1/panic")
	if p := res.problem(t); res.Status != http.StatusInternalServerError || p["code"] != codeInternal || strings.Contains(res.Body, "boom") {
		t.Fatalf("panic: %d %v", res.Status, p)
	}
	if res := ts.get(t, "/api/v1/ping"); res.Status != http.StatusOK {
		t.Fatalf("after the panic: %d", res.Status)
	}
}

func TestDecodeJSONBoundsTheBodyAndRejectsUnknownFields(t *testing.T) {
	ts := newPingServer(t)
	res := ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`{"a":1}`))
	if res.Status != http.StatusAccepted || strings.TrimSpace(res.Body) != `{"a":1}` {
		t.Fatalf("a good body: %d %q", res.Status, res.Body)
	}
	res = ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`{"a":1,"b":2}`))
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidBody {
		t.Fatalf("unknown field: %d %v", res.Status, p)
	}
	res = ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`{"a":1} {"a":2}`))
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidBody {
		t.Fatalf("two objects: %d %v", res.Status, p)
	}
	res = ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`not json`))
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidBody {
		t.Fatalf("not json: %d %v", res.Status, p)
	}
	big := `{"a":1,"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	res = ts.do(t, http.MethodPost, "/api/v1/echo", testToken, bytes.NewReader([]byte(big)))
	if p := res.problem(t); res.Status != http.StatusRequestEntityTooLarge || p["code"] != codePayloadTooLarge {
		t.Fatalf("too big: %d %v", res.Status, p)
	}
}

func TestMutationsAreAuditedWithTheirFieldsAndReadsAreNot(t *testing.T) {
	ts := newPingServer(t)
	ts.get(t, "/api/v1/ping")
	ts.do(t, http.MethodGet, "/healthz", "", nil)
	ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`{"a":1}`), "User-Agent", "delamain-app/1.0")
	ts.do(t, http.MethodPost, "/api/v1/echo", testToken, strings.NewReader(`{"zzz":1}`))
	lines := ts.auditLines(t)
	if len(lines) != 2 {
		t.Fatalf("two mutations, no reads: %v", lines)
	}
	l := lines[0]
	if l["event"] != eventMutation || l["method"] != "POST" || l["path"] != "/api/v1/echo" || l["status"] != float64(202) || l["run_id"] != float64(7) || l["user_agent"] != "delamain-app/1.0" {
		t.Fatalf("audit line = %v", l)
	}
	if _, ok := l["duration_ms"]; !ok {
		t.Fatalf("the line has a duration: %v", l)
	}
	if l := lines[1]; l["status"] != float64(400) || l["error"] != codeInvalidBody {
		t.Fatalf("a refused mutation is audited with its problem code: %v", l)
	}
	if strings.Contains(string(mustRead(t, ts.audit)), `"a":1`) || strings.Contains(string(mustRead(t, ts.audit)), "zzz") {
		t.Fatal("the audit never holds a body")
	}
	for path, want := range map[string]os.FileMode{ts.audit: 0o600, filepath.Dir(ts.audit): 0o700} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s is %v, want %v: %v", path, fi.Mode().Perm(), want, err)
		}
	}
}

func TestMapErrShouldTurnTheTypedErrorsIntoProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
		extra  map[string]any
	}{
		"not found":    {&control.RunError{ID: 9, Err: control.ErrNotFound}, 404, codeNotFound, nil},
		"invalid":      {control.ErrInvalid, 400, codeInvalidParam, nil},
		"cursor":       {control.ErrInvalidCursor, 400, codeInvalidCursor, nil},
		"repo":         {control.ErrRepoNotAllowed, 403, codeRepoNotAllowed, nil},
		"review off":   {control.ErrReviewDisabled, 409, codeReviewDisabled, nil},
		"no forge":     {control.ErrNoForge, 409, codeNoForge, nil},
		"closed":       {&control.IssueError{Issue: 4, Err: control.ErrIssueClosed}, 409, codeIssueClosed, map[string]any{"issue": 4}},
		"active":       {&control.RunError{ID: 3, State: store.StateImplement, Ref: "#4", Err: control.ErrRunActive}, 409, codeRunActive, map[string]any{"run_id": int64(3), "state": "implement"}},
		"finished":     {&control.RunError{ID: 3, State: store.StateRecorded, Err: control.ErrRunFinished}, 409, codeRunFinished, map[string]any{"run_id": int64(3), "state": "recorded"}},
		"not finished": {&control.RunError{ID: 3, State: store.StatePlan, Err: control.ErrRunNotFinished}, 409, codeRunNotFinished, map[string]any{"run_id": int64(3), "state": "plan"}},
		"owner alive":  {&control.RunError{ID: 3, OwnerPID: 42, Err: control.ErrOwnerAlive}, 409, codeOwnerAlive, map[string]any{"run_id": int64(3), "owner_pid": 42}},
		"dirty":        {&control.RunError{ID: 3, Worktree: "/wt", Err: control.ErrDirtyWorktree}, 409, codeWorktreeDirty, map[string]any{"run_id": int64(3), "worktree": "/wt"}},
		"in use":       {&control.RunError{ID: 3, UsedBy: 5, State: store.StateImplement, Worktree: "/wt", Err: control.ErrWorktreeInUse}, 409, codeWorktreeInUse, map[string]any{"run_id": int64(5), "worktree": "/wt", "state": "implement"}},
		"forge 404":    {&control.IssueError{Issue: 4, Err: &forge.APIError{Status: 404}}, 404, codeIssueNotFound, nil},
		"forge 500":    {&forge.APIError{Status: 502}, 502, codeForgeError, map[string]any{"forge_status": 502}},
		"problem":      {&problemError{status: 418, code: "teapot", detail: "x"}, 418, "teapot", nil},
		"other":        {errors.New("boom"), 500, codeInternal, nil},
	} {
		t.Run(name, func(t *testing.T) {
			p := mapErr(tc.err)
			if p.status != tc.status || p.code != tc.code {
				t.Fatalf("got %d %s, want %d %s", p.status, p.code, tc.status, tc.code)
			}
			for k, v := range tc.extra {
				if got, ok := p.extra[k]; !ok || got != v {
					t.Fatalf("extra[%s] = %v (%T), want %v (%T)", k, got, got, v, v)
				}
			}
		})
	}
	if !contains(Codes, codeInternal) || len(Codes) < 20 {
		t.Fatal("Codes lists every code")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The first streams of a fresh server may open at once: the gate must neither let more through
// than its bound nor lose a release and hang.
func TestStreamGateShouldHoldItsBoundWhenTheFirstStreamsOpenAtOnce(t *testing.T) {
	for range 50 {
		g := &streamGate{max: 3}
		var held, peak atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 16 {
			wg.Go(func() {
				<-start
				release, ok := g.take()
				if !ok {
					return
				}
				n := held.Add(1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				held.Add(-1)
				release()
			})
		}
		close(start)
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a take or release hung")
		}
		if peak.Load() > 3 {
			t.Fatalf("%d streams held at once, the bound is 3", peak.Load())
		}
		if _, ok := g.take(); !ok {
			t.Fatal("every release gave its stream back")
		}
	}
}

func TestStreamGateShouldBoundTheOpenStreams(t *testing.T) {
	g := &streamGate{max: 2}
	r1, ok1 := g.take()
	r2, ok2 := g.take()
	_, ok3 := g.take()
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("two streams fit, the third does not: %v %v %v", ok1, ok2, ok3)
	}
	r1()
	if _, ok := g.take(); !ok {
		t.Fatal("a released stream makes room")
	}
	r2()
}

func TestNewShouldDefaultAZeroLimitAndKeepANegativeOneAsNone(t *testing.T) {
	for limit, want := range map[int]int{0: 20, -1: -1, 5: 5} {
		if got := New(&fakeSource{}, nil, nil, Options{Token: []byte(testToken), Limit: limit}).opts.Limit; got != want {
			t.Fatalf("Limit %d: the server keeps %d, want %d", limit, got, want)
		}
	}
}
