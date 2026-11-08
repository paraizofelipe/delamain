// Package api serves `delamain serve`: the remote-control API a phone reaches through an HTTPS
// tunnel. It listens on loopback only and a bearer token guards every route; the tunnel
// terminates TLS. Reads come from the same store the board and the watch read; actions go
// through internal/control, as the CLI's do.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/control"
)

// Source is everything the API reads: what the board reads, plus the repositories it may act on
// and the runs page by page.
type Source interface {
	board.Source
	// Repos lists the repositories the API may start or queue, with their forge when known.
	Repos(ctx context.Context) ([]Repo, error)
	// RunsBefore pages the runs newest first: up to limit with an id below before (0 means from
	// the newest), filtered by "active", "finished" or "all".
	RunsBefore(ctx context.Context, before int64, limit int, filter string) ([]board.Run, error)
}

// Options tune a Server.
type Options struct {
	// Token is the bearer token every route demands; serve refuses to start without one.
	Token []byte
	// MaxStreams bounds the open event and log streams.
	MaxStreams int
	// TrustProxy takes the client address from CF-Connecting-IP or X-Forwarded-For when the peer
	// is loopback: through a tunnel every request comes from 127.0.0.1.
	TrustProxy bool
	// AuthFailures per client in authWindow before it is locked out for authLockout.
	AuthFailures int
	// RequestsPerSecond over every route, with a burst of twice that.
	RequestsPerSecond int
	// Interval is how often the event hub reloads while a stream is open.
	Interval time.Duration
	// Limit is how many finished runs the event stream and the board projection carry.
	Limit   int
	Version string
	Log     *slog.Logger
	// Now dates the lockouts and the audit; tests replace it.
	Now func() time.Time
}

// Limits of a request.
const (
	maxBodyBytes      = 64 << 10
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 16 << 10
)

// Server is the API.
type Server struct {
	opts    Options
	src     Source
	acts    *control.Actions
	audit   *Audit
	board   *board.Builder
	auth    *authGuard
	limiter *rateLimiter
	streams *streamGate
	log     *slog.Logger
	// testRoutes are extra handlers tests register, such as one that panics.
	testRoutes map[string]http.HandlerFunc
}

// New builds the API over src and acts, auditing to audit (nil audits nothing).
func New(src Source, acts *control.Actions, audit *Audit, opts Options) *Server {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = 8
	}
	if opts.AuthFailures <= 0 {
		opts.AuthFailures = 10
	}
	if opts.RequestsPerSecond <= 0 {
		opts.RequestsPerSecond = 20
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	return &Server{
		opts: opts, src: src, acts: acts, audit: audit, log: opts.Log, board: board.NewBuilder(src, opts.Limit),
		auth:    newAuthGuard(opts.Token, opts.AuthFailures, opts.Now),
		limiter: newRateLimiter(float64(opts.RequestsPerSecond), 2*opts.RequestsPerSecond, opts.Now),
		streams: &streamGate{max: opts.MaxStreams},
	}
}

// Serve runs an http.Server on ln until ctx ends, with the timeouts a public tunnel calls for;
// the write timeout stays off because of the event streams, which end with ctx.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	hs := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Run drives what the API does between requests, until ctx ends: the event hub, once it exists.
func (s *Server) Run(ctx context.Context) {
	<-ctx.Done()
}

// Handler is the API: the routes under /api/v1 behind the token, and /healthz in front of it.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	api := http.NewServeMux()
	for pattern, h := range s.testRoutes {
		api.HandleFunc(pattern, h)
	}
	s.routes(api)
	mux.Handle("/api/v1/", s.authenticate(s.serveMux(api)))
	// audited wraps throttle so a refused burst gets its audit line too.
	return s.recoverPanics(s.headers(s.audited(s.throttle(s.serveMux(mux)))))
}

// routes registers the API routes on mux.
func (s *Server) routes(mux *http.ServeMux) {
	s.readRoutes(mux)
}

// serveMux serves mux, answering what matches nothing with a problem instead of plain text.
func (s *Server) serveMux(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		var allowed []string
		for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
			probe := r.Clone(r.Context())
			probe.Method = m
			if _, pattern := mux.Handler(probe); pattern != "" {
				allowed = append(allowed, m)
			}
		}
		if len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			writeProblem(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path), nil)
			return
		}
		writeProblem(w, r, http.StatusNotFound, codeNotFound, "no such route", nil)
	})
}

// headers sets what every response carries: nothing is cached, sniffed or framed.
func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// recoverPanics turns a panicking handler into a 500 and keeps the server up.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("api: handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeProblem(w, r, http.StatusInternalServerError, codeInternal, "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// throttle refuses what exceeds the global request rate.
func (s *Server) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.allow() {
			noteAudit(r, "event", eventRateLimited)
			w.Header().Set("Retry-After", "1")
			writeProblem(w, r, http.StatusTooManyRequests, codeRateLimited, "too many requests", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate demands the bearer token, locking out a client that fails too often.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := clientIP(r, s.opts.TrustProxy)
		switch ok, retry := s.auth.check(client, r.Header.Get("Authorization")); {
		case retry > 0:
			noteAudit(r, "event", eventRateLimited)
			w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())))
			writeProblem(w, r, http.StatusTooManyRequests, codeRateLimited, "too many failed logins; try later", nil)
			return
		case !ok:
			noteAudit(r, "event", eventAuthFailed)
			w.Header().Set("WWW-Authenticate", `Bearer realm="delamain"`)
			writeProblem(w, r, http.StatusUnauthorized, codeUnauthorized, "a valid bearer token is required", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the peer's address, or the tunnel's idea of the client when the peer is loopback
// and the proxy is trusted.
func clientIP(r *http.Request, trustProxy bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !trustProxy {
		return host
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return host
}

// writeJSON answers v as JSON with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads the request body into v: at most maxBodyBytes, no unknown fields.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return &problemError{status: http.StatusRequestEntityTooLarge, code: codePayloadTooLarge, detail: fmt.Sprintf("the body must be at most %d bytes", maxBodyBytes)}
		}
		return &problemError{status: http.StatusBadRequest, code: codeInvalidBody, detail: "invalid JSON body: " + err.Error()}
	}
	if dec.More() {
		return &problemError{status: http.StatusBadRequest, code: codeInvalidBody, detail: "the body must be one JSON object"}
	}
	return nil
}

// streamGate bounds the open streams.
type streamGate struct {
	mu   chan struct{}
	max  int
	open int
}

// take reserves a stream, or reports that none is left; release gives it back.
func (g *streamGate) take() (release func(), ok bool) {
	g.lock()
	defer g.unlock()
	if g.open >= g.max {
		return nil, false
	}
	g.open++
	return func() {
		g.lock()
		g.open--
		g.unlock()
	}, true
}

func (g *streamGate) lock() {
	if g.mu == nil {
		g.mu = make(chan struct{}, 1)
	}
	g.mu <- struct{}{}
}

func (g *streamGate) unlock() { <-g.mu }
