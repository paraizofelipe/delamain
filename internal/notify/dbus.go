package notify

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

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
	Capabilities(ctx context.Context) ([]string, error)
	ServerInfo(ctx context.Context) (name, version string, err error)
	// Subscribe delivers the server's ActionInvoked and NotificationClosed signals until Close.
	Subscribe() (<-chan signal, error)
	Notify(ctx context.Context, n notification) (uint32, error)
	Close() error
}

const (
	actionInvoked      = "ActionInvoked"
	notificationClosed = "NotificationClosed"
)

// signal is an ActionInvoked (with the action key) or a NotificationClosed.
type signal struct {
	name string
	id   uint32
	key  string
}

var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// actionsTTL is how long a toast's actions stay clickable when the server never says it closed.
const actionsTTL = 24 * time.Hour

// dbusBackend sends toasts to org.freedesktop.Notifications, one per run. In the daemon, when the
// server supports actions, it also opens what a click asks for, if it is a target it trusts.
type dbusBackend struct {
	bus     bus
	icon    func() string
	log     *slog.Logger
	actions bool // interactive and the server supports actions
	click   clickTargets
	open    func(ctx context.Context, target string) error
	now     func() time.Time

	mu      sync.Mutex
	toasts  map[int64]uint32 // run → its notification id
	pending map[uint32]clickable
}

type clickable struct {
	actions []Action
	at      time.Time
}

func newDBus(b bus, icon func() string, s sys, opts Options) *dbusBackend {
	d := &dbusBackend{
		bus: b, icon: icon, log: opts.Log, open: s.open, now: s.now,
		click:  clickTargets{hosts: opts.ForgeHosts, stateDir: opts.StateDir},
		toasts: map[int64]uint32{}, pending: map[uint32]clickable{},
	}
	if !opts.Interactive {
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	caps, err := b.Capabilities(ctx)
	if err != nil || !slices.Contains(caps, "actions") {
		return d
	}
	signals, err := b.Subscribe()
	if err != nil {
		d.log.Warn("cannot listen for notification clicks", "err", err)
		return d
	}
	d.actions = true
	go d.listen(signals)
	return d
}

func (d *dbusBackend) send(ctx context.Context, m Message) error {
	icon := d.icon()
	actions := []string{}
	if d.actions {
		for _, a := range m.Actions {
			actions = append(actions, a.Key, a.Label)
		}
	}
	d.mu.Lock()
	replaces := d.toasts[m.RunID]
	d.mu.Unlock()
	id, err := d.bus.Notify(ctx, notification{
		AppName: "delamain", ReplacesID: replaces, AppIcon: icon,
		Summary: markup.Replace(m.Title), Body: markup.Replace(m.Body), Actions: actions,
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
	d.mu.Lock()
	defer d.mu.Unlock()
	if m.RunID != 0 {
		d.toasts[m.RunID] = id
	}
	if d.actions {
		d.forgetExpired()
		delete(d.pending, id)
		if len(m.Actions) > 0 {
			d.pending[id] = clickable{actions: m.Actions, at: d.now()}
		}
	}
	return nil
}

// listen handles clicks until the bus closes the signal channel.
func (d *dbusBackend) listen(signals <-chan signal) {
	for s := range signals {
		switch s.name {
		case notificationClosed:
			d.mu.Lock()
			delete(d.pending, s.id)
			d.mu.Unlock()
		case actionInvoked:
			if target, ok := d.target(s.id, s.key); ok {
				d.openTarget(target)
			}
		}
	}
}

// target is what the action key of a still-clickable toast points at.
func (d *dbusBackend) target(id uint32, key string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forgetExpired()
	for _, a := range d.pending[id].actions {
		if a.Key == key {
			return a.Target, true
		}
	}
	return "", false // another application's toast, or one already forgotten
}

func (d *dbusBackend) openTarget(target string) {
	if !d.click.allowed(target) {
		d.log.Warn("notification click ignored: untrusted target", "target", target)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := d.open(ctx, target); err != nil {
		d.log.Warn("could not open the notification target", "target", target, "err", err)
	}
}

// forgetExpired drops actions older than actionsTTL; the caller holds d.mu.
func (d *dbusBackend) forgetExpired() {
	now := d.now()
	for id, c := range d.pending {
		if now.Sub(c.at) > actionsTTL {
			delete(d.pending, id)
		}
	}
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

func (b connBus) Capabilities(ctx context.Context) ([]string, error) {
	var caps []string
	err := b.conn.Object(notificationsName, notificationsPath).CallWithContext(ctx, notificationsName+".GetCapabilities", 0).Store(&caps)
	return caps, err
}

func (b connBus) Subscribe() (<-chan signal, error) {
	if err := b.conn.AddMatchSignal(dbus.WithMatchObjectPath(notificationsPath), dbus.WithMatchInterface(notificationsName)); err != nil {
		return nil, err
	}
	in, out := make(chan *dbus.Signal, 16), make(chan signal)
	b.conn.Signal(in)
	go func() {
		defer close(out)
		for s := range in { // closed when the connection closes
			var sig signal
			switch {
			case s.Name == notificationsName+"."+actionInvoked && len(s.Body) == 2:
				sig.name = actionInvoked
				sig.id, _ = s.Body[0].(uint32)
				sig.key, _ = s.Body[1].(string)
			case s.Name == notificationsName+"."+notificationClosed && len(s.Body) >= 1:
				sig.name = notificationClosed
				sig.id, _ = s.Body[0].(uint32)
			default:
				continue
			}
			out <- sig
		}
	}()
	return out, nil
}

func (b connBus) ServerInfo(ctx context.Context) (string, string, error) {
	var name, vendor, version, spec string
	err := b.conn.Object(notificationsName, notificationsPath).CallWithContext(ctx, notificationsName+".GetServerInformation", 0).
		Store(&name, &vendor, &version, &spec)
	return name, version, err
}
