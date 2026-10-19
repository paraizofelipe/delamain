package notify

import (
	"context"
	"strings"
	"sync"
)

// bus is the part of the session bus the D-Bus backend talks to (org.freedesktop.Notifications).
type bus interface {
	NameHasOwner(ctx context.Context, name string) (bool, error)
	Notify(ctx context.Context, n notification) (uint32, error)
	Close() error
}

// notification is the argument list of org.freedesktop.Notifications.Notify.
type notification struct {
	AppName    string
	ReplacesID uint32
	AppIcon    string
	Summary    string
	Body       string
	Actions    []string
	Hints      map[string]any
	Expire     int32
}

// dbusBackend sends toasts to any freedesktop notification server (GNOME, KDE, mako, dunst...).
type dbusBackend struct {
	bus  bus
	opts Options

	mu    sync.Mutex
	byRun map[int64]uint32 // RunID -> id of the run's toast, replaced by its next one
}

func newDBus(b bus, o Options) *dbusBackend {
	return &dbusBackend{bus: b, opts: o, byRun: map[int64]uint32{}}
}

// markup escapes what servers that read body markup would interpret.
var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (d *dbusBackend) Notify(ctx context.Context, m Message) error {
	icon, err := writeIcon(d.opts.StateDir)
	if err != nil {
		return err
	}
	d.mu.Lock()
	replaces := d.byRun[m.RunID]
	d.mu.Unlock()
	id, err := d.bus.Notify(ctx, notification{
		AppName: "delamain", ReplacesID: replaces, AppIcon: icon,
		Summary: markup.Replace(m.Title), Body: markup.Replace(m.Body),
		Hints:  map[string]any{"urgency": byte(m.Urgency), "category": m.Category, "image-path": icon},
		Expire: m.Expire,
	})
	if err != nil {
		return err
	}
	if m.RunID != 0 {
		d.mu.Lock()
		d.byRun[m.RunID] = id
		d.mu.Unlock()
	}
	return nil
}

func (d *dbusBackend) Close() error { return d.bus.Close() }
