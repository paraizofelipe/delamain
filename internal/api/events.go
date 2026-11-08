package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/control"
)

// eventBuffer is how many events a subscriber may fall behind before it is dropped: a phone
// that stopped reading reconnects and gets the full state again, which beats blocking the hub.
var eventBuffer = 64

// sseMsg is one event, already encoded.
type sseMsg struct {
	name string
	data json.RawMessage
}

// subscriber is one open event stream.
type subscriber struct {
	ch      chan sseMsg
	dropped bool
}

// hub reloads the state while a stream is open and pushes to every stream what changed since
// the last reload: a run by run, the queue and the status whole, operations as they move.
type hub struct {
	s    *Server
	kick chan struct{}

	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	runs   map[int64]json.RawMessage // last run summaries, encoded, by id
	queue  json.RawMessage
	status json.RawMessage
}

func newHub(s *Server) *hub {
	return &hub{s: s, kick: make(chan struct{}, 1), subs: map[*subscriber]struct{}{}}
}

// hello opens every stream.
type hello struct {
	ServerTime time.Time `json:"server_time"`
	Version    string    `json:"version"`
	IntervalMS int64     `json:"interval_ms"`
}

// run reloads every interval while a stream is open, and forwards the operations.
func (h *hub) run(ctx context.Context) {
	t := time.NewTicker(h.s.opts.Interval)
	defer t.Stop()
	var ops <-chan control.Operation
	if h.s.acts != nil {
		ch, unsub := h.s.acts.Operations().Subscribe()
		defer unsub()
		ops = ch
	}
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-ops:
			h.publish("operation", operationOf(op))
		case <-t.C:
			if h.watched() {
				h.reload(ctx)
			}
		case <-h.kick:
			if h.watched() {
				h.reload(ctx)
			}
		}
	}
}

// watched reports whether a stream is open; with none, the state is forgotten so the next
// stream never diffs against a board minutes old.
func (h *hub) watched() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		h.runs, h.queue, h.status = nil, nil, nil
		return false
	}
	return true
}

// current reads the state as it stands: the run summaries, the queue and the status, encoded.
func (h *hub) current(ctx context.Context) (runs map[int64]json.RawMessage, ordered []RunSummary, queue, status json.RawMessage, err error) {
	st, err := h.s.src.Status(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	status, _ = json.Marshal(h.s.statusOf(st, false))
	q, err := h.s.src.Queue(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	queue, _ = json.Marshal(list[QueueItem]{Items: queueOf(q)})
	rs, err := h.s.src.Runs(ctx, h.s.opts.Limit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	runs = make(map[int64]json.RawMessage, len(rs))
	ordered = make([]RunSummary, 0, len(rs))
	for _, r := range rs {
		sum := summaryOf(r, h.s.board)
		ordered = append(ordered, sum)
		runs[r.ID], _ = json.Marshal(sum)
	}
	return runs, ordered, queue, status, nil
}

// reload reads the state and pushes what differs from the last reload.
func (h *hub) reload(ctx context.Context) {
	runs, _, queue, status, err := h.current(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			h.send("problem", mustJSON(map[string]string{"code": codeInternal, "detail": err.Error()}))
		}
		return
	}
	if h.runs == nil { // the first reload after the streams opened sent the full state themselves
		h.runs, h.queue, h.status = runs, queue, status
		return
	}
	for id, data := range runs {
		if !bytes.Equal(h.runs[id], data) {
			h.send("run", data)
		}
	}
	for id := range h.runs {
		if _, ok := runs[id]; !ok {
			h.send("run_removed", mustJSON(map[string]int64{"id": id}))
		}
	}
	if !bytes.Equal(h.queue, queue) {
		h.send("queue", queue)
	}
	if !bytes.Equal(h.status, status) {
		h.send("status", status)
	}
	h.runs, h.queue, h.status = runs, queue, status
}

// publish encodes v and pushes it to every stream.
func (h *hub) publish(name string, v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	h.send(name, mustJSON(v))
}

// send pushes one event to every stream, dropping the ones that fell too far behind; the
// caller holds the mutex, and nothing here waits on a stream.
func (h *hub) send(name string, data json.RawMessage) {
	for sub := range h.subs {
		select {
		case sub.ch <- sseMsg{name, data}:
		default:
			sub.dropped = true
			close(sub.ch)
			delete(h.subs, sub)
		}
	}
}

// subscribe opens a stream: it reads the state once, records it as the last one and returns
// it with the subscriber, so the loop's next reload diffs against what the stream got.
func (h *hub) subscribe(ctx context.Context) (*subscriber, []sseMsg, error) {
	runs, ordered, queue, status, err := h.current(ctx)
	if err != nil {
		return nil, nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs, h.queue, h.status = runs, queue, status
	sub := &subscriber{ch: make(chan sseMsg, eventBuffer)}
	h.subs[sub] = struct{}{}
	select {
	case h.kick <- struct{}{}:
	default:
	}
	initial := []sseMsg{
		{"hello", mustJSON(hello{ServerTime: h.s.opts.Now(), Version: h.s.opts.Version, IntervalMS: h.s.opts.Interval.Milliseconds()})},
		{"runs", mustJSON(list[RunSummary]{Items: ordered})},
		{"queue", queue},
		{"status", status},
	}
	return sub, initial, nil
}

func (h *hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, sub)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return b
}

func (s *Server) eventRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
}

// handleEvents streams the state: hello, the runs, the queue and the status first, then what
// changes, run by run.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	release, ok := s.streams.take()
	if !ok {
		writeProblem(w, r, http.StatusTooManyRequests, codeTooManyStreams, "too many open streams", nil)
		return
	}
	defer release()
	sub, initial, err := s.hub.subscribe(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	defer s.hub.unsubscribe(sub)
	sse := startSSE(w, r)
	if sse == nil {
		return
	}
	for _, m := range initial {
		if err := sse.raw(m.name, m.data); err != nil {
			return
		}
	}
	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if err := sse.ping(); err != nil {
				return
			}
		case m, open := <-sub.ch:
			if !open {
				_ = sse.raw("problem", mustJSON(map[string]string{"code": codeTooManyStreams, "detail": "the stream fell behind; reconnect"}))
				return
			}
			if err := sse.raw(m.name, m.data); err != nil {
				return
			}
		}
	}
}
