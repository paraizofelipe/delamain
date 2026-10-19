package notify

import (
	"context"
	"strings"
	"sync"

	"github.com/godbus/dbus/v5"
)

// notification is one org.freedesktop.Notifications.Notify call.
type notification struct {
	AppName    string
	ReplacesID uint32
	AppIcon    string
	Summary    string
	Body       string
	Actions    []string
	Hints      map[string]dbus.Variant
	Expire     int32
}

// bus is the session bus with a notification server on it.
type bus interface {
	Notify(ctx context.Context, n notification) (uint32, error)
	Close() error
}

var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// dbusBackend sends toasts to org.freedesktop.Notifications, one per run.
type dbusBackend struct {
	bus  bus
	icon func() string

	mu     sync.Mutex
	toasts map[int64]uint32 // run → its notification id
}

func newDBus(b bus, icon func() string) *dbusBackend {
	return &dbusBackend{bus: b, icon: icon, toasts: map[int64]uint32{}}
}

func (d *dbusBackend) send(ctx context.Context, m Message) error {
	icon := d.icon()
	d.mu.Lock()
	replaces := d.toasts[m.RunID]
	d.mu.Unlock()
	id, err := d.bus.Notify(ctx, notification{
		AppName: "delamain", ReplacesID: replaces, AppIcon: icon,
		Summary: markup.Replace(m.Title), Body: markup.Replace(m.Body), Actions: []string{},
		Hints: map[string]dbus.Variant{
			"urgency":    dbus.MakeVariant(byte(m.Urgency)),
			"category":   dbus.MakeVariant(m.Category),
			"image-path": dbus.MakeVariant(icon),
		},
		Expire: m.Expire,
	})
	if err != nil {
		return err
	}
	if m.RunID != 0 {
		d.mu.Lock()
		d.toasts[m.RunID] = id
		d.mu.Unlock()
	}
	return nil
}

func (d *dbusBackend) close() error { return d.bus.Close() }
