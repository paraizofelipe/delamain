// Package notify shows native desktop notifications: D-Bus on Linux, Notification Center on
// macOS. A notification never fails a run: errors and timeouts become warnings in the log.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
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
	// ForgeHosts are the hosts whose https URLs a click may open.
	ForgeHosts []string
}

// timeout bounds each toast: a stuck notification server never holds a run.
const timeout = 5 * time.Second

// New resolves the configured backend once; without a server it sends nothing and warns once.
func New(cfg config.Notify, opts Options) Notifier {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return newWith(cfg, opts, realSys())
}

func realSys() sys {
	return sys{
		goos: runtime.GOOS, getenv: os.Getenv, dial: dialBus, lookPath: exec.LookPath, run: runCommand,
		open: xdgOpen, now: time.Now,
	}
}

// runCommand runs a program directly: its arguments never reach a shell.
func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed programs, text passed as argv
	if err != nil && len(out) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return err
}

// sys is what the notifier touches outside the process; tests replace it.
type sys struct {
	goos   string
	getenv func(string) string
	dial   func(ctx context.Context, addr string) (bus, error)
	// lookPath and run execute a program directly, without a shell.
	lookPath func(string) (string, error)
	run      func(ctx context.Context, name string, args ...string) error
	// open shows a clicked target (xdg-open); now dates the actions it may still open.
	open func(ctx context.Context, target string) error
	now  func() time.Time
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
	kind    string

	iconOnce sync.Once
	iconPath string
}

func newWith(cfg config.Notify, opts Options, s sys) *Client {
	c := &Client{log: opts.Log, backend: noBackend{}, kind: config.NotifyNone}
	if !cfg.Enabled {
		return c
	}
	c.events = map[Event]bool{}
	for _, e := range cfg.Events {
		c.events[Event(e)] = true
	}
	icon := func() string { return c.icon(opts.StateDir) }
	kind, b, err := resolve(cfg.Backend, s)
	switch kind {
	case config.NotifyMacOS:
		c.backend = newMacOS(s, icon, opts)
	case config.NotifyDBus:
		c.backend = newDBus(b, icon, s, opts)
	}
	c.kind = kind
	if err != nil {
		c.log.Warn("no desktop notification server; notifications disabled", "err", err)
	}
	return c
}

// Notify sends the toast of an enabled event. It never fails: errors become warnings.
func (c *Client) Notify(ctx context.Context, m Message) error {
	if !c.events[m.Event] {
		return nil
	}
	if err := c.Send(ctx, m); err != nil {
		c.log.Warn("desktop notification failed", "event", m.Event, "err", err)
	}
	return nil
}

// Send delivers a toast whatever the configured events, and returns why it failed.
func (c *Client) Send(ctx context.Context, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.backend.send(ctx, m)
}

// Backend is the resolved backend: dbus, macos or none.
func (c *Client) Backend() string { return c.kind }

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
