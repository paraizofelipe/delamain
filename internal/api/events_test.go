package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
)

// names are the event names of evs, in order.
func names(evs []sseEvent) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// setRun replaces run id of src with mutate applied.
func (s *fakeSource) setRun(id int64, mutate func(*board.Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == id {
			mutate(&s.runs[i])
		}
	}
}

func (s *fakeSource) removeRun(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == id {
			s.runs = append(s.runs[:i], s.runs[i+1:]...)
			return
		}
	}
}

func (s *fakeSource) setQueue(q []board.QueueItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = q
}

func (s *fakeSource) setStatus(st board.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

func TestEventsShouldOpenWithTheFullStateThenPushOnlyWhatChanged(t *testing.T) {
	src := &fakeSource{runs: sampleRuns(t.TempDir()), status: board.Status{Daemon: true, SlotsMax: 3, TodayLevel: "ok"},
		queue: []board.QueueItem{{ID: 1, Position: 1, Repo: "/r/app", Issue: 9, Title: "q"}}}
	ts := newTestServer(t, src)
	st := ts.openStream(t, "/api/v1/events")
	if st.res.StatusCode != http.StatusOK || st.res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %v", st.res.StatusCode, st.res.Header)
	}
	first := st.next(t, 4)
	if names(first) != "hello,runs,queue,status" {
		t.Fatalf("the opening burst: %s", names(first))
	}
	var h hello
	if err := json.Unmarshal([]byte(first[0].Data), &h); err != nil || h.Version != "vtest" || h.IntervalMS != 20 || !h.ServerTime.Equal(ts.clock.now()) {
		t.Fatalf("hello = %s (%v)", first[0].Data, err)
	}
	var runs list[RunSummary]
	if err := json.Unmarshal([]byte(first[1].Data), &runs); err != nil || len(runs.Items) != 4 {
		t.Fatalf("runs = %s", first[1].Data)
	}
	if !strings.Contains(first[2].Data, `"id":1`) || !strings.Contains(first[3].Data, `"daemon":true`) || strings.Contains(first[3].Data, "server_time") {
		t.Fatalf("queue and status, the status without a server time: %s %s", first[2].Data, first[3].Data)
	}

	// Nothing changed: nothing is pushed.
	select {
	case ev := <-st.events:
		t.Fatalf("an unchanged state pushes nothing, got %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}

	src.setRun(2, func(r *board.Run) { r.State, r.Iteration = "verify", 2 })
	ev := st.next(t, 1)[0]
	if ev.Name != "run" || !strings.Contains(ev.Data, `"id":2`) || !strings.Contains(ev.Data, `"state":"verify"`) || !strings.Contains(ev.Data, `"column":"verify"`) {
		t.Fatalf("a changed run is pushed alone: %+v", ev)
	}

	src.removeRun(4)
	ev = st.next(t, 1)[0]
	if ev.Name != "run_removed" || ev.Data != `{"id":4}` {
		t.Fatalf("a run that left the window: %+v", ev)
	}

	src.setQueue(nil)
	ev = st.next(t, 1)[0]
	if ev.Name != "queue" || ev.Data != `{"items":[]}` {
		t.Fatalf("the whole queue when it changes: %+v", ev)
	}

	src.setStatus(board.Status{Daemon: false, SlotsMax: 3, TodayLevel: "ok"})
	ev = st.next(t, 1)[0]
	if ev.Name != "status" || !strings.Contains(ev.Data, `"daemon":false`) {
		t.Fatalf("the status when it changes: %+v", ev)
	}

	// A second stream gets the current state, not the diffs the first one saw.
	second := ts.openStream(t, "/api/v1/events")
	burst := second.next(t, 4)
	if names(burst) != "hello,runs,queue,status" || !strings.Contains(burst[1].Data, `"state":"verify"`) || strings.Contains(burst[1].Data, `"id":4`) {
		t.Fatalf("second stream: %s %s", names(burst), burst[1].Data)
	}
}

func TestEventsShouldReportAReloadThatFails(t *testing.T) {
	src := &fakeSource{runs: sampleRuns(t.TempDir())}
	ts := newTestServer(t, src)
	st := ts.openStream(t, "/api/v1/events")
	st.next(t, 4)
	src.mu.Lock()
	src.err = http.ErrAbortHandler
	src.mu.Unlock()
	ev := st.next(t, 1)[0]
	if ev.Name != "problem" || !strings.Contains(ev.Data, codeInternal) {
		t.Fatalf("a failing reload is a problem event: %+v", ev)
	}
	broken := ts.openStream(t, "/api/v1/events")
	if broken.res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a stream that cannot load its state is refused: %d", broken.res.StatusCode)
	}
}

func TestEventsShouldDropASlowSubscriberWithoutBlockingTheOthers(t *testing.T) {
	prev := eventBuffer
	eventBuffer = 2
	t.Cleanup(func() { eventBuffer = prev })
	src := &fakeSource{runs: sampleRuns(t.TempDir())}
	ts := newTestServer(t, src)
	slow := ts.openStream(t, "/api/v1/events")
	slow.next(t, 4)
	fast := ts.openStream(t, "/api/v1/events")
	fast.next(t, 4)

	// The slow stream's goroutine keeps reading into its channel, so fill it by stopping the read:
	// close its body and make changes until the hub notices it fell behind.
	_ = slow.res.Body.Close()
	for i := range 20 {
		src.setRun(2, func(r *board.Run) { r.Iteration = i + 10 })
		time.Sleep(25 * time.Millisecond)
	}
	got := fast.next(t, 1)
	if got[0].Name != "run" {
		t.Fatalf("the fast stream keeps getting events: %+v", got)
	}
	ts.srv.hub.mu.Lock()
	n := len(ts.srv.hub.subs)
	ts.srv.hub.mu.Unlock()
	if n != 1 {
		t.Fatalf("the closed stream is gone, the fast one stays: %d subscribers", n)
	}
}

func TestEventsCountAgainstMaxStreams(t *testing.T) {
	ts := newTestServer(t, &fakeSource{}, func(o *Options) { o.MaxStreams = 2 })
	a := ts.openStream(t, "/api/v1/events")
	b := ts.openStream(t, "/api/v1/events")
	if a.res.StatusCode != http.StatusOK || b.res.StatusCode != http.StatusOK {
		t.Fatalf("two streams fit: %d %d", a.res.StatusCode, b.res.StatusCode)
	}
	c := ts.openStream(t, "/api/v1/events")
	if c.res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("the third does not: %d", c.res.StatusCode)
	}
	if res := ts.get(t, "/api/v1/status"); res.Status != http.StatusOK {
		t.Fatalf("plain reads are not streams: %d", res.Status)
	}
}

func TestHubShouldForgetTheStateWhenNoStreamIsOpen(t *testing.T) {
	src := &fakeSource{runs: sampleRuns(t.TempDir())}
	ts := newTestServer(t, src)
	st := ts.openStream(t, "/api/v1/events")
	st.next(t, 4)
	_ = st.res.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ts.srv.hub.mu.Lock()
		gone := len(ts.srv.hub.subs) == 0 && ts.srv.hub.runs == nil
		ts.srv.hub.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("with no stream open the hub forgets the state and stops reloading")
		}
		time.Sleep(10 * time.Millisecond)
	}
	src.mu.Lock()
	calls := src.calls
	src.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	src.mu.Lock()
	defer src.mu.Unlock()
	if src.calls != calls {
		t.Fatalf("the hub reloads only while a stream is open: %d → %d", calls, src.calls)
	}
}
