package api

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
)

func appendLog(t *testing.T, dir, source, text string) {
	t.Helper()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	fh, err := os.OpenFile(filepath.Join(logs, source+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// finish records the run id of src as finished.
func (s *fakeSource) finish(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == id {
			s.runs[i].State, s.runs[i].Reason = "recorded", "approved: ok"
		}
	}
}

func TestLogsShouldTailAndContinueFromTheCursor(t *testing.T) {
	dir := t.TempDir()
	appendLog(t, dir, "delamain", "10:00:00 state run=2 state=plan\n10:00:01 state run=2 state=implement\n")
	appendLog(t, dir, "implementer", "10:00:02 ▶ prompt\n10:00:03 🔧 Bash: go te")
	src := &fakeSource{runs: []board.Run{{ID: 2, Repo: "/r/app", Issue: "#2", State: "implement", OwnerPID: 1, Dir: dir}}}
	ts := newTestServer(t, src)

	res := ts.get(t, "/api/v1/runs/2/logs?tail=1")
	var page logsResponse
	res.json(t, &page)
	if res.Status != http.StatusOK || len(page.Lines) != 1 || page.Lines[0].Text != "10:00:02 ▶ prompt" || page.Cursor == "" || page.Finished {
		t.Fatalf("the tail is the last line, the partial one held: %+v", page)
	}
	if page.Lines[0] != (LogLine{TS: "10:00:02", Source: "implementer", Text: "10:00:02 ▶ prompt"}) {
		t.Fatalf("line = %+v", page.Lines[0])
	}

	appendLog(t, dir, "implementer", "st ./...\n")
	appendLog(t, dir, "delamain", "10:00:04 state run=2 state=verify\n")
	res = ts.get(t, "/api/v1/runs/2/logs?cursor="+page.Cursor)
	page = logsResponse{}
	res.json(t, &page)
	if len(page.Lines) != 2 || page.Lines[0].Text != "10:00:03 🔧 Bash: go test ./..." || page.Lines[1].Text != "10:00:04 state run=2 state=verify" {
		t.Fatalf("from the cursor: the completed line and the new one: %+v", page.Lines)
	}

	appendLog(t, dir, "delamain", "10:00:05 delivered url=x")
	src.finish(2)
	res = ts.get(t, "/api/v1/runs/2/logs?cursor="+page.Cursor+"&roles=delamain")
	page = logsResponse{}
	res.json(t, &page)
	if !page.Finished || len(page.Lines) != 1 || page.Lines[0].Text != "10:00:05 delivered url=x" {
		t.Fatalf("a finished run flushes its last line: %+v", page)
	}
	res = ts.get(t, "/api/v1/runs/2/logs?cursor="+page.Cursor)
	page = logsResponse{}
	res.json(t, &page)
	if len(page.Lines) != 0 || !page.Finished {
		t.Fatalf("nothing twice: %+v", page)
	}
}

func TestLogsShouldRefuseABadCursorBadRolesAndAnUnknownRun(t *testing.T) {
	src := &fakeSource{runs: []board.Run{{ID: 2, State: "plan", Dir: t.TempDir()}}}
	ts := newTestServer(t, src)
	res := ts.get(t, "/api/v1/runs/2/logs?cursor=not-a-cursor!")
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidCursor {
		t.Fatalf("bad cursor: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/runs/2/logs?roles=../etc")
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidParam {
		t.Fatalf("bad roles: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/runs/2/logs?tail=-1")
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidParam {
		t.Fatalf("bad tail: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/runs/9/logs")
	if p := res.problem(t); res.Status != http.StatusNotFound || p["code"] != codeNotFound {
		t.Fatalf("unknown run: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/runs/2/logs")
	var page logsResponse
	res.json(t, &page)
	if res.Status != http.StatusOK || len(page.Lines) != 0 || page.Cursor != "" {
		t.Fatalf("no logs yet is an empty page: %d %+v", res.Status, page)
	}
}

// sseEvent is one event of a stream: its name and data line.
type sseEvent struct{ Name, Data string }

// sseStream is an open event stream, read by one goroutine so no event is lost between reads.
type sseStream struct {
	res    *http.Response
	events chan sseEvent
	errc   chan error
}

// openStream opens an SSE stream; the response is closed with the test.
func (ts *testServer) openStream(t *testing.T, path string) *sseStream { //nolint:unparam // the events tests open another path
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	st := &sseStream{res: res, events: make(chan sseEvent, 64), errc: make(chan error, 1)}
	go func() {
		r := bufio.NewReader(res.Body)
		var ev sseEvent
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				st.errc <- err
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.Name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.Data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.Name != "":
				st.events <- ev
				ev = sseEvent{}
			}
		}
	}()
	return st
}

// next waits for want more events.
func (st *sseStream) next(t *testing.T, want int, timeout time.Duration) []sseEvent {
	t.Helper()
	var out []sseEvent
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev := <-st.events:
			out = append(out, ev)
		case err := <-st.errc:
			t.Fatalf("stream ended after %d events: %v", len(out), err)
		case <-deadline:
			t.Fatalf("got %d of %d events: %+v", len(out), want, out)
		}
	}
	return out
}

// ended reports whether the stream closed within timeout.
func (st *sseStream) ended(timeout time.Duration) bool {
	select {
	case <-st.errc:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestLogStreamShouldPushLinesThenEndWhenTheRunFinishes(t *testing.T) {
	dir := t.TempDir()
	appendLog(t, dir, "delamain", "10:00:00 state run=2 state=plan\n")
	src := &fakeSource{runs: []board.Run{{ID: 2, State: "plan", OwnerPID: 1, Dir: dir}}}
	ts := newTestServer(t, src)
	ts.srv.logPoll, ts.srv.logStateCheck = 5*time.Millisecond, 10*time.Millisecond

	st := ts.openStream(t, "/api/v1/runs/2/logs/stream")
	res := st.res
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("stream headers: %d %v", res.StatusCode, res.Header)
	}
	first := st.next(t, 1, 5*time.Second)
	if first[0].Name != "log" || !strings.Contains(first[0].Data, "state=plan") || !strings.Contains(first[0].Data, `"cursor":"`) {
		t.Fatalf("the existing lines come first: %+v", first)
	}
	appendLog(t, dir, "checks", "10:00:01 ▶ test: go test\nok\n")
	next := st.next(t, 1, 5*time.Second)
	if next[0].Name != "log" || !strings.Contains(next[0].Data, `"source":"checks"`) || strings.Count(next[0].Data, `"text"`) != 2 {
		t.Fatalf("appended lines are pushed: %+v", next)
	}
	appendLog(t, dir, "delamain", "10:00:02 delivered")
	src.finish(2)
	last := st.next(t, 2, 5*time.Second)
	if last[0].Name != "log" || !strings.Contains(last[0].Data, "delivered") || last[1].Name != "end" || !strings.Contains(last[1].Data, `"cursor":"`) {
		t.Fatalf("the last line is flushed and end closes the stream: %+v", last)
	}
	if !st.ended(5 * time.Second) {
		t.Fatal("the stream ends after end")
	}
}

func TestLogStreamsCountAgainstMaxStreams(t *testing.T) {
	src := &fakeSource{runs: []board.Run{{ID: 2, State: "plan", OwnerPID: 1, Dir: t.TempDir()}}}
	ts := newTestServer(t, src, func(o *Options) { o.MaxStreams = 1 })
	first := ts.openStream(t, "/api/v1/runs/2/logs/stream")
	if first.res.StatusCode != http.StatusOK {
		t.Fatalf("first stream: %d", first.res.StatusCode)
	}
	second := ts.openStream(t, "/api/v1/runs/2/logs/stream")
	if second.res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second stream: %d", second.res.StatusCode)
	}
	if !second.ended(5*time.Second) || !strings.HasPrefix(second.res.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("the second stream is a closed problem: %v", second.res.Header)
	}
	if plain := ts.get(t, "/api/v1/runs/2/logs"); plain.Status != http.StatusOK {
		t.Fatalf("a plain read is not a stream: %d", plain.Status)
	}
	_ = first.res.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		third := ts.openStream(t, "/api/v1/runs/2/logs/stream")
		if third.res.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed stream frees its slot: %d", third.res.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
