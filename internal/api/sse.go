package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// keepAlive is how often an idle stream gets a comment, so the tunnel and the client keep it:
// Cloudflare drops a connection idle for 100 seconds.
const keepAlive = 15 * time.Second

// sseWriter writes Server-Sent Events to a response.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// startSSE sets the stream headers and answers 200; nil when the connection cannot stream.
func startSSE(w http.ResponseWriter, r *http.Request) *sseWriter {
	f, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, r, http.StatusInternalServerError, codeInternal, "streaming is not supported here", nil)
		return nil
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}
}

// event writes one named event with v as its JSON data.
func (s *sseWriter) event(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.raw(name, data)
}

// raw writes one named event with data already encoded.
func (s *sseWriter) raw(name string, data []byte) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// ping writes the keep-alive comment.
func (s *sseWriter) ping() error {
	if _, err := fmt.Fprint(s.w, ": ping\n\n"); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
