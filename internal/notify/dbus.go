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
	NameHasOwner(name string) (bool, error)
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

const notificationsPath = dbus.ObjectPath("/org/freedesktop/Notifications")

// connBus is the real session bus.
type connBus struct{ conn *dbus.Conn }

func dialBus(addr string) (bus, error) {
	conn, err := dbus.Connect(addr)
	if err != nil {
		return nil, err
	}
	return connBus{conn}, nil
}

func (b connBus) NameHasOwner(name string) (bool, error) {
	var owned bool
	err := b.conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, name).Store(&owned)
	return owned, err
}

func (b connBus) Notify(ctx context.Context, n notification) (uint32, error) {
	var id uint32
	err := b.conn.Object(notificationsName, notificationsPath).CallWithContext(ctx, notificationsName+".Notify", 0,
		n.AppName, n.ReplacesID, n.AppIcon, n.Summary, n.Body, n.Actions, n.Hints, n.Expire).Store(&id)
	return id, err
}

func (b connBus) Close() error { return b.conn.Close() }
