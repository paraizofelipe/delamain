package notify

import (
	"context"

	"github.com/godbus/dbus/v5"
)

const (
	busName = "org.freedesktop.Notifications"
	busPath = dbus.ObjectPath("/org/freedesktop/Notifications")
)

// godbusConn is the real session bus, in pure Go (no CGO).
type godbusConn struct {
	conn *dbus.Conn
}

func dialBus(addr string) (bus, error) {
	c, err := dbus.Connect(addr)
	if err != nil {
		return nil, err
	}
	return &godbusConn{conn: c}, nil
}

func (g *godbusConn) obj() dbus.BusObject { return g.conn.Object(busName, busPath) }

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

func (g *godbusConn) Close() error { return g.conn.Close() }
