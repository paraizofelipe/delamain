package notify

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// macosBackend shows toasts in the Notification Center: with terminal-notifier when installed
// (logo, grouping, click), else with osascript (no logo, no click).
type macosBackend struct {
	opts             Options
	terminalNotifier string // path; empty falls back to osascript
}

func newMacOS(o Options) *macosBackend {
	o = o.withDefaults()
	tn, err := o.lookPath("terminal-notifier")
	if err != nil {
		tn = ""
	}
	return &macosBackend{opts: o, terminalNotifier: tn}
}

func (m *macosBackend) Notify(ctx context.Context, msg Message) error {
	// The Notification Center shows two lines at most.
	body := strings.ReplaceAll(msg.Body, "\n", " · ")
	if m.terminalNotifier == "" {
		// Third-party text (the issue title) only ever travels as argv: never inside the script.
		return m.opts.run(ctx, "osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run",
			"--", msg.Title, body)
	}
	icon, err := writeIcon(m.opts.StateDir)
	if err != nil {
		return err
	}
	args := []string{"-title", msg.Title, "-message", body}
	if msg.RunID != 0 {
		args = append(args, "-group", fmt.Sprintf("delamain-run-%d", msg.RunID)) // replaces the run's last toast
	}
	args = append(args, "-contentImage", icon)
	if url := m.clickURL(msg); url != "" {
		args = append(args, "-open", url)
	}
	if msg.Urgency == Critical {
		args = append(args, "-sound", "default")
	}
	return m.opts.run(ctx, m.terminalNotifier, args...)
}

// clickURL is the default action's URL when the daemon may open it on click.
func (m *macosBackend) clickURL(msg Message) string {
	if !m.opts.Interactive {
		return ""
	}
	for _, a := range msg.Actions {
		if a.Key == "default" && strings.HasPrefix(a.Target, "https://") && validTarget(a.Target, msg.ForgeHost, m.opts.StateDir) == nil {
			return a.Target
		}
	}
	return ""
}

func (m *macosBackend) Close() error { return nil }

func execRun(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed binaries, text as argv
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
