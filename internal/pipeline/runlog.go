package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// lineHandler writes the orchestrator's log of one run as "15:04:05 msg k=v", the same time
// prefix the agent logs use, so `delamain logs` can interleave them.
type lineHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	attrs []slog.Attr
}

func newLineHandler(w io.Writer) *lineHandler { return &lineHandler{mu: &sync.Mutex{}, w: w} }

func (h *lineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05"))
	if r.Level != slog.LevelInfo {
		b.WriteString(" " + r.Level.String())
	}
	b.WriteString(" " + r.Message)
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%s", a.Key, logValue(a.Value.Resolve().String()))
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *lineHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &lineHandler{mu: h.mu, w: h.w, attrs: append(h.attrs[:len(h.attrs):len(h.attrs)], as...)}
}

func (h *lineHandler) WithGroup(string) slog.Handler { return h }

func logValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		return fmt.Sprintf("%q", v)
	}
	return v
}

// teeHandler sends every record to all handlers that want it.
type teeHandler []slog.Handler

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range t {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}
