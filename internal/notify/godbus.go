package notify

import (
	"context"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	busName = "org.freedesktop.Notifications"
	busPath = dbus.ObjectPath("/org/freedesktop/Notifications")
)

// godbusConn is the real session bus, in pure Go (no CGO).
type godbusConn struct {
	conn   *dbus.Conn
	closed chan struct{}
	once   sync.Once
}

func dialBus(addr string) (bus, error) {
	c, err := dbus.Connect(addr)
	if err != nil {
		return nil, err
	}
	return &godbusConn{conn: c, closed: make(chan struct{})}, nil
}

func (g *godbusConn) obj() dbus.BusObject { return g.conn.Object(busName, busPath) }

func (g *godbusConn) NameHasOwner(ctx context.Context, name string) (bool, error) {
	var owned bool
	err := g.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, name).Store(&owned)
	return owned, err
}

func (g *godbusConn) Notify(ctx context.Context, n notification) (uint32, error) {
	hints := make(map[string]dbus.Variant, len(n.Hints))
	for k, v := range n.Hints {
		hints[k] = dbus.MakeVariant(v)
	}
	actions := n.Actions
	if actions == nil {
		actions = []string{}
	}
	var id uint32
	err := g.obj().CallWithContext(ctx, busName+".Notify", 0,
		n.AppName, n.ReplacesID, n.AppIcon, n.Summary, n.Body, actions, hints, n.Expire).Store(&id)
	return id, err
}

func (g *godbusConn) Capabilities(ctx context.Context) ([]string, error) {
	var caps []string
	err := g.obj().CallWithContext(ctx, busName+".GetCapabilities", 0).Store(&caps)
	return caps, err
}

func (g *godbusConn) Subscribe() (<-chan busSignal, error) {
	if err := g.conn.AddMatchSignal(dbus.WithMatchObjectPath(busPath), dbus.WithMatchInterface(busName)); err != nil {
		return nil, err
	}
	in, out := make(chan *dbus.Signal, 16), make(chan busSignal)
	g.conn.Signal(in)
	go func() {
		defer close(out)
		for s := range in { // closed when the connection closes
			b, ok := toBusSignal(s)
			if !ok {
				continue
			}
			select {
			case out <- b:
			case <-g.closed:
				return
			}
		}
	}()
	return out, nil
}

// toBusSignal reads ActionInvoked(id, key) and NotificationClosed(id, reason).
func toBusSignal(s *dbus.Signal) (busSignal, bool) {
	if s.Path != busPath || len(s.Body) < 2 {
		return busSignal{}, false
	}
	id, ok := s.Body[0].(uint32)
	if !ok {
		return busSignal{}, false
	}
	switch s.Name {
	case busName + "." + actionInvoked:
		key, ok := s.Body[1].(string)
		return busSignal{Name: actionInvoked, ID: id, Key: key}, ok
	case busName + "." + notificationClosed:
		return busSignal{Name: notificationClosed, ID: id}, true
	}
	return busSignal{}, false
}

func (g *godbusConn) Close() error {
	g.once.Do(func() { close(g.closed) })
	return g.conn.Close()
}
