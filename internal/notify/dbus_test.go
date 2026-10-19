package notify

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// fakeBus records what a notification server would receive.
type fakeBus struct {
	mu     sync.Mutex
	sent   []notification
	nextID uint32
}

func (b *fakeBus) Notify(_ context.Context, n notification) (uint32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, n)
	b.nextID++
	return b.nextID, nil
}

func (b *fakeBus) Close() error { return nil }

func (b *fakeBus) last(t *testing.T) notification {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sent) == 0 {
		t.Fatal("nothing reached the notification server")
	}
	return b.sent[len(b.sent)-1]
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestDBusShouldSendAppIconImagePathUrgencyAndCategory(t *testing.T) {
	state := t.TempDir()
	b := &fakeBus{}
	d := newDBus(b, Options{StateDir: state, Log: discard()})
	if err := d.Notify(context.Background(), FormatPROpened(example())); err != nil {
		t.Fatal(err)
	}

	icon := filepath.Join(state, "notify", "delamain.png")
	n := b.last(t)
	if n.AppName != "delamain" || n.AppIcon != icon || n.Expire != 15000 {
		t.Fatalf("app_name=%q app_icon=%q expire=%d", n.AppName, n.AppIcon, n.Expire)
	}
	if n.Summary != "✅ PR opened · delamain #12" || n.Body != "Mostrar HARNESS e INTERACTIONS no status\nPR #31 · 2 iterations · 167K tokens · US$ 0.67" {
		t.Fatalf("summary=%q body=%q", n.Summary, n.Body)
	}
	want := map[string]any{"urgency": byte(1), "category": "x-delamain.pr", "image-path": icon}
	if !reflect.DeepEqual(n.Hints, want) {
		t.Fatalf("hints = %#v, want %#v", n.Hints, want)
	}
}
