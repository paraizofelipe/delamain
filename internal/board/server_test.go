package board

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T, src Source, localOnly bool) *httptest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(src, Options{Interval: 20 * time.Millisecond, Limit: 20, LocalOnly: localOnly})
	go srv.Run(ctx)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	ts.Start()
	t.Cleanup(func() {
		cancel()
		ts.Close()
	})
	return ts
}

// response is what a GET answered, with its body already read and closed.
type response struct {
	StatusCode int
	Header     http.Header
}

func get(t *testing.T, url string) (response, string) {
	t.Helper()
	res, err := http.Get(url) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return response{StatusCode: res.StatusCode, Header: res.Header}, string(body)
}

func TestServesThePageAndTheAPI(t *testing.T) {
	src := &fakeSource{runs: []Run{{ID: 1, State: "plan", Repo: "/r/app", Dir: t.TempDir()}}}
	ts := newTestServer(t, src, true)

	res, body := get(t, ts.URL+"/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "board.js") {
		t.Fatalf("page: %d %q", res.StatusCode, body)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("no nosniff")
	}
	for _, asset := range []string{"/board.js", "/board.css"} {
		if res, _ := get(t, ts.URL+asset); res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", asset, res.StatusCode)
		}
	}

	res, body = get(t, ts.URL+"/api/snapshot")
	var snap Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("snapshot: %d %v %q", res.StatusCode, err, body)
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("the API is cacheable")
	}
	if len(snap.Columns[2].Cards) != 1 || snap.Columns[2].Cards[0].ID != 1 {
		t.Fatalf("plan column = %+v", snap.Columns[2])
	}

	if res, _ := get(t, ts.URL+"/api/runs/1"); res.StatusCode != http.StatusOK {
		t.Fatalf("run 1: %d", res.StatusCode)
	}
	if res, _ := get(t, ts.URL+"/api/runs/abc"); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("run abc: %d", res.StatusCode)
	}
	if res, _ := get(t, ts.URL+"/api/runs/42"); res.StatusCode != http.StatusNotFound {
		t.Fatalf("run 42: %d", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/snapshot", nil) //nolint:noctx // test
	post, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", post.StatusCode)
	}
}

func TestServesTheWaitingLookOfAQueuedCard(t *testing.T) {
	ts := newTestServer(t, &fakeSource{}, true)

	_, js := get(t, ts.URL+"/board.js")
	for _, want := range []string{"&& 'waiting'", "'⏳ aguardando '"} {
		if !strings.Contains(js, want) {
			t.Errorf("board.js has no %q", want)
		}
	}
	if strings.Contains(js, "innerHTML") {
		t.Error("board.js writes HTML: the waiting reason comes from an issue and must stay text")
	}
	_, css := get(t, ts.URL+"/board.css")
	for _, want := range []string{".card.queued.waiting {", ".card.queued.waiting:hover", ".tag.waiting {"} {
		if !strings.Contains(css, want) {
			t.Errorf("board.css has no %q", want)
		}
	}
}

func TestRefusesAForeignHost(t *testing.T) {
	ts := newTestServer(t, &fakeSource{}, true)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/snapshot", nil) //nolint:noctx // test
	req.Host = "evil.example:7777"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
}

func TestReportRouteShouldServeTheSourceReport(t *testing.T) {
	src := &fakeSource{report: Report{
		Period:      "30d",
		Repos:       []string{"/r/app"},
		KPIs:        ReportKPIs{Runs: 3, CostUSD: 4.5},
		Daily:       []ReportDay{},
		TopRuns:     []ReportRun{{ID: 7, Ref: "#12", Title: "Login", CostUSD: 2.25}},
		TopRoles:    []ReportRole{},
		TopSessions: []ReportSession{},
		History:     []ReportRun{},
	}}
	ts := newTestServer(t, src, true)

	res, body := get(t, ts.URL+"/api/report")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("report: %d %q", res.StatusCode, body)
	}
	for _, want := range []string{
		`"period":"30d"`, `"repos":["/r/app"]`, `"kpis":{"runs":3,"cost_usd":4.5,`,
		`"top_runs":[{"id":7,"ref":"#12","title":"Login",`, `"cost_usd":2.25`, `"history":[]`, `"next_offset":null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no %s in %s", want, body)
		}
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("no nosniff")
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("the report is cacheable")
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.query != (ReportQuery{Period: "30d"}) {
		t.Errorf("the source got %+v", src.query)
	}
}

func TestReportRouteShouldAnswer500WhenTheSourceFails(t *testing.T) {
	ts := newTestServer(t, &fakeSource{err: errors.New("database is locked")}, true)
	res, body := get(t, ts.URL+"/api/report")
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "database is locked") {
		t.Fatalf("report: %d %q", res.StatusCode, body)
	}
}

func TestReportRouteShouldAnswer400ToABadQuery(t *testing.T) {
	src := &fakeSource{}
	ts := newTestServer(t, src, true)
	for _, query := range []string{"period=1y", "offset=-1", "offset=abc"} {
		res, body := get(t, ts.URL+"/api/report?"+query)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(body, "inválido") {
			t.Errorf("%s: %d %q", query, res.StatusCode, body)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: Content-Type = %q", query, ct)
		}
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.query != (ReportQuery{}) {
		t.Errorf("the source was asked for %+v", src.query)
	}
}

func TestReportRouteShouldRefuseAForeignHost(t *testing.T) {
	src := &fakeSource{}
	ts := newTestServer(t, src, true)
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/report", nil) //nolint:noctx // test
	req.Host = "evil.example:7777"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", res.StatusCode)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.query != (ReportQuery{}) {
		t.Errorf("the source was asked for %+v", src.query)
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:7777": true, "localhost:7777": true, "LOCALHOST": true, "[::1]:7777": true,
		"evil.example:7777": false, "192.168.0.2:7777": false, "127.0.0.1.evil.example": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v", host, got)
		}
	}
}

type sseEvent struct{ name, data string }

// readEvents parses an event stream into events, skipping keep-alive comments.
func readEvents(r io.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 16)
	go func() {
		defer close(out)
		var e sseEvent
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				e.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				e.data = strings.TrimPrefix(line, "data: ")
			case line == "" && e.name != "":
				out <- e
				e = sseEvent{}
			}
		}
	}()
	return out
}

func next(events <-chan sseEvent, timeout time.Duration) (string, string) {
	select {
	case e := <-events:
		return e.name, e.data
	case <-time.After(timeout):
		return "", ""
	}
}

func TestEventsPushOnlyWhatChanged(t *testing.T) {
	src := &fakeSource{runs: []Run{{ID: 1, State: "plan", Dir: t.TempDir()}}}
	ts := newTestServer(t, src, false)
	res, err := http.Get(ts.URL + "/events") //nolint:noctx // closed with the server
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events := readEvents(res.Body)

	name, data := next(events, 2*time.Second)
	if name != "snapshot" || !strings.Contains(data, `"key":"run-1"`) {
		t.Fatalf("first event %q %q", name, data)
	}
	// Nothing changed: several ticks go by without an event.
	if name, _ := next(events, 150*time.Millisecond); name != "" {
		t.Fatalf("event without a change: %q", name)
	}

	src.set(func(f *fakeSource) { f.runs[0].State = "implement" })
	name, data = next(events, 2*time.Second)
	var snap Snapshot
	if err := json.Unmarshal([]byte(data), &snap); name != "snapshot" || err != nil {
		t.Fatalf("event after the change %q %v", name, err)
	}
	if len(snap.Columns[3].Cards) != 1 {
		t.Fatalf("implement column = %+v", snap.Columns[3])
	}

	src.set(func(f *fakeSource) { f.err = context.DeadlineExceeded })
	if name, data := next(events, 2*time.Second); name != "problem" || !strings.Contains(data, "deadline") {
		t.Fatalf("event of a failed reload %q %q", name, data)
	}
}
