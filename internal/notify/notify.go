// Package notify shows native desktop notifications (D-Bus on Linux, Notification Center on
// macOS) for what a run, the daemon or the budget wants a human to know. A notification never
// fails its caller: errors are logged and the work goes on.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"time"

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

// Urgency follows the freedesktop spec: 0 low, 1 normal, 2 critical.
type Urgency byte

const (
	Low Urgency = iota
	Normal
	Critical
)

// Action is a button of the toast; Key "default" is the click on the toast itself.
type Action struct {
	Key    string // "default", "open-pr", "open-issue", "open-report"
	Label  string // "Open PR", "Open issue", "Open report"
	Target string // https URL (PR/issue) or the absolute path of report.md
}

type Message struct {
	Event Event
	RunID int64 // 0 for events that are not about one run (budget, recover)
	Title string
	Body  string // lines separated by "\n", plain text
	// Urgency and Expire (ms; 0 never expires, critical only) follow the freedesktop spec.
	Urgency  Urgency
	Expire   int32
	Category string // "x-delamain.<...>"
	Actions  []Action
	// ForgeHost is the host of the run's forge: a URL action only opens on that host.
	ForgeHost string
}

type Notifier interface {
	Notify(ctx context.Context, m Message) error
	Close() error
}

// Options configure New. Interactive is true only in the daemon: only there a click can open
// the PR, the issue or the report.
type Options struct {
	Interactive bool
	StateDir    string
	Log         *slog.Logger

	// Seams for tests; zero values are the real ones.
	goos    string
	getenv  func(string) string
	dial    func(addr string) (bus, error)
	timeout time.Duration
}

// Allowed reports whether cfg lets event e be shown.
func Allowed(cfg config.Notify, e Event) bool {
	return cfg.Enabled && slices.Contains(cfg.Events, string(e))
}

// timeout bounds every toast: a stuck notification server never holds a run.
const timeout = 5 * time.Second

// New resolves the configured backend. auto picks macos on darwin, else dbus when a server owns
// org.freedesktop.Notifications on the session bus, else none with a single warning.
func New(cfg config.Notify, o Options) Notifier {
	o = o.withDefaults()
	name, b := resolve(cfg, o)
	return &safe{backend: b, name: name, log: o.Log, timeout: o.timeout}
}

func resolve(cfg config.Notify, o Options) (string, Notifier) {
	if !cfg.Enabled || cfg.Backend == config.NotifyNone {
		return config.NotifyNone, none{}
	}
	if cfg.Backend == config.NotifyMacOS || (cfg.Backend == config.NotifyAuto && o.goos == "darwin") {
		return config.NotifyMacOS, newMacOS(o)
	}
	b, err := connectSessionBus(o)
	if err != nil {
		o.Log.Warn("no desktop notification server; notifications disabled", "err", err)
		return config.NotifyNone, none{}
	}
	return config.NotifyDBus, newDBus(b, o)
}

// connectSessionBus connects to the session bus and checks that a notification server is on it.
func connectSessionBus(o Options) (bus, error) {
	addr := o.getenv("DBUS_SESSION_BUS_ADDRESS")
	if xdg := o.getenv("XDG_RUNTIME_DIR"); addr == "" && xdg != "" {
		addr = "unix:path=" + xdg + "/bus"
	}
	if addr == "" {
		return nil, errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS and XDG_RUNTIME_DIR are unset)")
	}
	b, err := o.dial(addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	owned, err := b.NameHasOwner(ctx, busName)
	if err == nil && !owned {
		err = fmt.Errorf("%s has no owner on %s", busName, addr)
	}
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

func (o Options) withDefaults() Options {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.getenv == nil {
		o.getenv = os.Getenv
	}
	if o.dial == nil {
		o.dial = dialBus
	}
	if o.timeout <= 0 {
		o.timeout = timeout
	}
	return o
}

// BackendOf names the backend n resolved to: dbus, macos or none.
func BackendOf(n Notifier) string {
	if b, ok := n.(interface{ Backend() string }); ok {
		return b.Backend()
	}
	return config.NotifyNone
}

// safe bounds each toast by the timeout and turns every failure into a warning.
type safe struct {
	backend Notifier
	name    string
	log     *slog.Logger
	timeout time.Duration
}

func (s *safe) Backend() string { return s.name }

func (s *safe) Notify(ctx context.Context, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.backend.Notify(ctx, m) }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		s.log.Warn("could not show desktop notification", "event", m.Event, "backend", s.name, "err", err)
	}
	return nil
}

func (s *safe) Close() error { return s.backend.Close() }

// none sends nothing: notifications are off or no server can show them.
type none struct{}

func (none) Notify(context.Context, Message) error { return nil }
func (none) Close() error                          { return nil }
