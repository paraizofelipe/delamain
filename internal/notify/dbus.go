package notify

import "context"

// bus is the part of the session bus the D-Bus backend talks to (org.freedesktop.Notifications).
type bus interface {
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
}

func newDBus(b bus, o Options) *dbusBackend {
	return &dbusBackend{bus: b, opts: o}
}

func (d *dbusBackend) Notify(ctx context.Context, m Message) error {
	icon, err := writeIcon(d.opts.StateDir)
	if err != nil {
		return err
	}
	_, err = d.bus.Notify(ctx, notification{
		AppName: "delamain", AppIcon: icon, Summary: m.Title, Body: m.Body,
		Hints:  map[string]any{"urgency": byte(m.Urgency), "category": m.Category, "image-path": icon},
		Expire: m.Expire,
	})
	return err
}

func (d *dbusBackend) Close() error { return d.bus.Close() }
