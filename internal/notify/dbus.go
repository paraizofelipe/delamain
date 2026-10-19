package notify

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// bus is the part of the session bus the D-Bus backend talks to (org.freedesktop.Notifications).
type bus interface {
	NameHasOwner(ctx context.Context, name string) (bool, error)
	Capabilities(ctx context.Context) ([]string, error)
	Notify(ctx context.Context, n notification) (uint32, error)
	// Subscribe delivers the server's ActionInvoked and NotificationClosed signals until Close.
	Subscribe() (<-chan busSignal, error)
	Close() error
}

// busSignal is an ActionInvoked (with the action key) or a NotificationClosed signal.
type busSignal struct {
	Name string
	ID   uint32
	Key  string
}

const (
	actionInvoked      = "ActionInvoked"
	notificationClosed = "NotificationClosed"
)

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

// actionsTTL is how long a toast's actions stay clickable when the server never says it closed.
const actionsTTL = 24 * time.Hour

// clickable is what a click on one toast may open.
type clickable struct {
	actions   []Action
	forgeHost string
	at        time.Time
}

// dbusBackend sends toasts to any freedesktop notification server (GNOME, KDE, mako, dunst...).
// In the daemon, on a server with actions, it also opens what a click points to.
type dbusBackend struct {
	bus     bus
	opts    Options
	actions bool // interactive and the server supports actions

	mu      sync.Mutex
	byRun   map[int64]uint32 // RunID -> id of the run's toast, replaced by its next one
	pending map[uint32]clickable
	done    chan struct{}
	stopped chan struct{}
}

func newDBus(b bus, o Options) *dbusBackend {
	o = o.withDefaults()
	d := &dbusBackend{bus: b, opts: o, byRun: map[int64]uint32{}, pending: map[uint32]clickable{}}
	if !o.Interactive {
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	caps, err := b.Capabilities(ctx)
	if err != nil {
		o.Log.Warn("notification server capabilities unknown; toasts get no buttons", "err", err)
		return d
	}
	if !slices.Contains(caps, "actions") {
		return d
	}
	signals, err := b.Subscribe()
	if err != nil {
		o.Log.Warn("cannot follow notification clicks; toasts get no buttons", "err", err)
		return d
	}
	d.actions, d.done, d.stopped = true, make(chan struct{}), make(chan struct{})
	go d.listen(signals)
	return d
}

// markup escapes what servers that read body markup would interpret.
var markup = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (d *dbusBackend) Notify(ctx context.Context, m Message) error {
	icon, err := writeIcon(d.opts.StateDir)
	if err != nil {
		return err
	}
	var actions []string
	if d.actions {
		for _, a := range m.Actions {
			actions = append(actions, a.Key, a.Label)
		}
	}
	d.mu.Lock()
	replaces := d.byRun[m.RunID]
	d.mu.Unlock()
	id, err := d.bus.Notify(ctx, notification{
		AppName: "delamain", ReplacesID: replaces, AppIcon: icon,
		Summary: markup.Replace(m.Title), Body: markup.Replace(m.Body), Actions: actions,
		Hints:  map[string]any{"urgency": byte(m.Urgency), "category": m.Category, "image-path": icon},
		Expire: m.Expire,
	})
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if m.RunID != 0 {
		d.byRun[m.RunID] = id
	}
	if len(actions) > 0 {
		d.forgetExpired()
		d.pending[id] = clickable{actions: m.Actions, forgeHost: m.ForgeHost, at: d.opts.now()}
	}
	return nil
}

// forgetExpired drops the actions of toasts older than actionsTTL. Callers hold d.mu.
func (d *dbusBackend) forgetExpired() {
	for id, c := range d.pending {
		if d.opts.now().Sub(c.at) > actionsTTL {
			delete(d.pending, id)
		}
	}
}

func (d *dbusBackend) listen(signals <-chan busSignal) {
	defer close(d.stopped)
	for {
		select {
		case <-d.done:
			return
		case s, ok := <-signals:
			if !ok {
				return
			}
			switch s.Name {
			case actionInvoked:
				d.invoke(s.ID, s.Key)
			case notificationClosed:
				d.mu.Lock()
				delete(d.pending, s.ID)
				d.mu.Unlock()
			}
		}
	}
}

// invoke opens the target of the clicked action, once it proves to be ours to open.
func (d *dbusBackend) invoke(id uint32, key string) {
	d.mu.Lock()
	d.forgetExpired()
	c, ok := d.pending[id]
	d.mu.Unlock()
	if !ok {
		return // another app's toast, or one already closed or expired
	}
	i := slices.IndexFunc(c.actions, func(a Action) bool { return a.Key == key })
	if i < 0 {
		return
	}
	target := c.actions[i].Target
	if err := validTarget(target, c.forgeHost, d.opts.StateDir); err != nil {
		d.opts.Log.Warn("notification click ignored", "target", target, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.opts.timeout)
	defer cancel()
	if err := d.opts.open(ctx, target); err != nil {
		d.opts.Log.Warn("could not open notification target", "target", target, "err", err)
	}
}

func (d *dbusBackend) Close() error {
	if d.done != nil {
		close(d.done)
		<-d.stopped
	}
	return d.bus.Close()
}
