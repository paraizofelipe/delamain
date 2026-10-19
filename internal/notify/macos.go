package notify

import (
	"context"
	"fmt"
	"strings"
)

// osascript reads the text from argv: a third-party issue title is never part of the script.
var osascript = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// macosBackend posts to Notification Center through terminal-notifier, or osascript without it.
type macosBackend struct {
	run         func(ctx context.Context, name string, args ...string) error
	notifier    string // terminal-notifier's path; "" falls back to osascript
	icon        func() string
	interactive bool
	hosts       []string
}

func newMacOS(s sys, icon func() string, opts Options) *macosBackend {
	m := &macosBackend{run: s.run, icon: icon, interactive: opts.Interactive, hosts: opts.ForgeHosts}
	if p, err := s.lookPath("terminal-notifier"); err == nil {
		m.notifier = p
	}
	return m
}

func (m *macosBackend) send(ctx context.Context, msg Message) error {
	// Notification Center shows at most two lines.
	body := strings.ReplaceAll(msg.Body, "\n", " · ")
	if m.notifier == "" {
		return m.run(ctx, "osascript", append(append([]string(nil), osascript...), "--", msg.Title, body)...)
	}
	args := []string{"-title", msg.Title, "-message", body}
	if msg.RunID != 0 {
		args = append(args, "-group", fmt.Sprintf("delamain-run-%d", msg.RunID))
	}
	args = append(args, "-contentImage", m.icon())
	if target := defaultTarget(msg); m.interactive && forgeURL(target, m.hosts) {
		args = append(args, "-open", target)
	}
	if msg.Urgency == Critical {
		args = append(args, "-sound", "default")
	}
	return m.run(ctx, m.notifier, args...)
}

func (m *macosBackend) close() error { return nil }

func defaultTarget(msg Message) string {
	for _, a := range msg.Actions {
		if a.Key == "default" {
			return a.Target
		}
	}
	return ""
}
