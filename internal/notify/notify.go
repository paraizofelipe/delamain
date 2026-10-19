// Package notify shows native desktop notifications (D-Bus on Linux, Notification Center on
// macOS) for what a run, the daemon or the budget wants a human to know. A notification never
// fails its caller: errors are logged and the work goes on.
package notify

import (
	"context"
	"log/slog"
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
}
