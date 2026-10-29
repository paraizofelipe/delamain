package board

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var web embed.FS

// Options tune a Server.
type Options struct {
	Interval time.Duration // between reloads
	Limit    int           // finished runs on the board
	// LocalOnly refuses requests whose Host is not a loopback name: a page of another site that
	// rebinds its DNS to 127.0.0.1 then cannot read the board.
	LocalOnly bool
}

// keepAlive is how often an idle event stream gets a comment, so proxies and browsers keep it.
const keepAlive = 15 * time.Second

// Server serves the board and pushes a snapshot to every open page when the board changes.
type Server struct {
	b    *builder
	opts Options
	kick chan struct{} // a page subscribed: reload now instead of on the next tick

	mu   sync.Mutex
	last event // what every new page gets first; empty until the first reload with a page open
	subs map[chan event]struct{}
}

type event struct {
	name string // snapshot | problem
	data []byte
}

func New(src Source, opts Options) *Server {
	return &Server{
		b:    &builder{src: src, files: newFiles(), limit: opts.Limit},
		opts: opts,
		kick: make(chan struct{}, 1),
		subs: map[chan event]struct{}{},
	}
}

// Run reloads the board every interval while a page is open, until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		if s.watched() {
			s.refresh(ctx)
		}
	}
}

func (s *Server) watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subs) == 0 {
		s.last = event{} // the next page must not get a board as it was minutes ago
		return false
	}
	return true
}

// refresh builds the board and sends it to the pages when it differs from the last one.
func (s *Server) refresh(ctx context.Context) {
	ev := event{name: "snapshot"}
	snap, err := s.b.snapshot(ctx)
	if err == nil {
		ev.data, err = json.Marshal(snap)
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		ev = event{name: "problem"}
		ev.data, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.name == s.last.name && bytes.Equal(ev.data, s.last.data) {
		return
	}
	s.last = ev
	for ch := range s.subs {
		send(ch, ev)
	}
}

// send never blocks: a page that has not read the previous board gets only the newest one.
func send(ch chan event, ev event) {
	select {
	case ch <- ev:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- ev:
	default:
	}
}

func (s *Server) subscribe() (chan event, func()) {
	ch := make(chan event, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	if s.last.name != "" {
		ch <- s.last
	}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Handler serves the page, its assets, the API and the event stream; every route only reads.
func (s *Server) Handler() http.Handler {
	assets, err := fs.Sub(web, "web")
	if err != nil {
		panic(err) // the embedded directory is part of the binary
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(assets))
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/runs/{id}", s.handleRun)
	mux.HandleFunc("GET /api/report", s.handleReport)
	mux.HandleFunc("GET /events", s.handleEvents)
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.LocalOnly && !loopbackHost(r.Host) {
			http.Error(w, "host não permitido", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.URL.Path == "/events" || strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackHost reports whether the Host header names this machine.
func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.Trim(hostport, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.b.snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, snap)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "id de run inválido", http.StatusBadRequest)
		return
	}
	d, err := s.b.detail(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, fmt.Sprintf("run %d não existe", id), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, d)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming não suportado", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch, unsubscribe := s.subscribe()
	defer unsubscribe()
	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		flusher.Flush()
	}
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	q, err := ParseReportQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rep, err := s.b.src.Report(r.Context(), q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
