package api

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Audit events.
const (
	eventMutation    = "mutation"
	eventAuthFailed  = "auth_failed"
	eventRateLimited = "rate_limited"
)

// Audit is the JSON-lines log of every mutation and every refused login: who asked what, when,
// and how it ended. It never holds the token or a body.
type Audit struct {
	mu  sync.Mutex
	f   *os.File
	log *slog.Logger
}

// OpenAudit appends to the file at path, created 0600 in a 0700 directory.
func OpenAudit(path string) (*Audit, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // under the state dir
	if err != nil {
		return nil, err
	}
	return &Audit{f: f, log: slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{ReplaceAttr: dropLevelAndMsg}))}, nil
}

// dropLevelAndMsg keeps the lines to their fields: the event is one of them.
func dropLevelAndMsg(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey || a.Key == slog.MessageKey {
		return slog.Attr{}
	}
	return a
}

func (a *Audit) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

func (a *Audit) write(attrs []slog.Attr) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.log.LogAttrs(context.Background(), slog.LevelInfo, "", attrs...)
}

// auditEntry collects what a request's handlers want on its audit line.
type auditEntry struct {
	mu    sync.Mutex
	attrs []slog.Attr
	event string
}

type auditKey struct{}

// noteAudit adds key=value to the audit line of r; "event" sets the event itself.
func noteAudit(r *http.Request, key string, value any) {
	e, ok := r.Context().Value(auditKey{}).(*auditEntry)
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if key == "event" {
		e.event, _ = value.(string)
		return
	}
	e.attrs = append(e.attrs, slog.Any(key, value))
}

// audited writes an audit line for every mutation, refused login and throttled request.
func (s *Server) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e := &auditEntry{}
		r = r.WithContext(context.WithValue(r.Context(), auditKey{}, e))
		sw := &statusWriter{ResponseWriter: w}
		start := s.opts.Now()
		next.ServeHTTP(sw, r)
		e.mu.Lock()
		defer e.mu.Unlock()
		mutation := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete
		if e.event == "" && mutation {
			e.event = eventMutation
		}
		if e.event == "" {
			return
		}
		attrs := []slog.Attr{
			slog.String("event", e.event), slog.String("method", r.Method), slog.String("path", r.URL.Path),
			slog.Int("status", sw.status()), slog.String("client_ip", clientIP(r, s.opts.TrustProxy)),
			slog.String("user_agent", r.UserAgent()), slog.Int64("duration_ms", s.opts.Now().Sub(start).Milliseconds()),
		}
		s.audit.write(append(attrs, e.attrs...))
	})
}

// statusWriter remembers the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets the event streams flush through the wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}
