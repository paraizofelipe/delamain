// Package notify shows native desktop notifications: D-Bus on Linux, Notification Center on
// macOS. A notification never fails a run: errors and timeouts become warnings in the log.
package notify

import (
	"context"
	"log/slog"
	"sync"

	"github.com/paraizo/delamain/internal/config"
)

type Event string

const (
	EventStarted    Event = "started" // off by default
	EventPROpened   Event = "pr_opened"
	EventNeedsHuman Event = "needs_human"
	EventHalted     Event = "halted"
	EventNeedsInfo  Event = "needs_info"
	EventBudget     Event = "budget" // soft and hard
	EventRecover    Event = "recover"
)

// Urgency follows the freedesktop spec.
type Urgency byte

const (
	Low Urgency = iota
	Normal
	Critical
)

type Action struct {
	Key    string // "default", "open-pr", "open-issue", "open-report"
	Label  string // "Open PR", "Open issue", "Open report"
	Target string // https URL (PR/issue) or the absolute path of report.md
}

type Message struct {
	Event    Event
	RunID    int64 // 0 for events that do not belong to a run (budget, recover)
	Title    string
	Body     string // lines separated by "\n", plain text
	Urgency  Urgency
	Expire   int32 // ms; 0 never expires (critical only)
	Category string
	Actions  []Action
}

type Notifier interface {
	Notify(ctx context.Context, m Message) error
	Close() error
}

type Options struct {
	// Interactive is true only in the daemon: toasts get actions that open their target.
	Interactive bool
	StateDir    string
	Log         *slog.Logger
}

// sys is what the notifier touches outside the process; tests replace it.
type sys struct {
	goos   string
	getenv func(string) string
	dial   func(addr string) (bus, error)
}

// backend delivers a toast on one platform.
type backend interface {
	send(ctx context.Context, m Message) error
	close() error
}

// noBackend drops every toast.
type noBackend struct{}

func (noBackend) send(context.Context, Message) error { return nil }
func (noBackend) close() error                        { return nil }

// Client is the Notifier New returns.
type Client struct {
	log     *slog.Logger
	backend backend
	events  map[Event]bool // nil when notifications are off

	iconOnce sync.Once
	iconPath string
}

func newWith(cfg config.Notify, opts Options, s sys) *Client {
	c := &Client{log: opts.Log, backend: noBackend{}}
	if !cfg.Enabled {
		return c
	}
	c.events = map[Event]bool{}
	for _, e := range cfg.Events {
		c.events[Event(e)] = true
	}
	icon := func() string { return c.icon(opts.StateDir) }
	if b, err := s.dial(s.getenv("DBUS_SESSION_BUS_ADDRESS")); err == nil {
		c.backend = newDBus(b, icon)
	}
	return c
}

// Notify sends the toast of an enabled event. It never fails: errors become warnings.
func (c *Client) Notify(ctx context.Context, m Message) error {
	if !c.events[m.Event] {
		return nil
	}
	if err := c.backend.send(ctx, m); err != nil {
		c.log.Warn("desktop notification failed", "event", m.Event, "err", err)
	}
	return nil
}

func (c *Client) Close() error { return c.backend.close() }

// icon writes the logo on the first toast; without it the toast still goes out.
func (c *Client) icon(stateDir string) string {
	c.iconOnce.Do(func() {
		p, err := writeIcon(stateDir)
		if err != nil {
			c.log.Warn("could not write the notification icon", "err", err)
		}
		c.iconPath = p
	})
	return c.iconPath
}
