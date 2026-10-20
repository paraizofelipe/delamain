package notify

import (
	"context"
	"slices"

	"github.com/paraizo/delamain/internal/config"
)

// Probe is what `delamain doctor` reports about desktop notifications.
type Probe struct {
	Backend          string // dbus, macos or none
	Server, Version  string // the D-Bus notification server
	Actions          bool   // the server supports actions (clicks)
	TerminalNotifier string // its path on macOS; "" means osascript
	Err              error  // why there is no backend
}

// Diagnose resolves the configured backend, as New does, and describes it.
func Diagnose(ctx context.Context, cfg config.Notify) Probe {
	return diagnose(ctx, cfg.Backend, realSys())
}

func diagnose(ctx context.Context, backend string, s sys) Probe {
	kind, b, err := resolve(backend, s)
	p := Probe{Backend: kind, Err: err}
	switch kind {
	case config.NotifyMacOS:
		if path, err := s.lookPath("terminal-notifier"); err == nil {
			p.TerminalNotifier = path
		}
	case config.NotifyDBus:
		defer func() { _ = b.Close() }()
		p.Server, p.Version, p.Err = b.ServerInfo(ctx)
		caps, _ := b.Capabilities(ctx)
		p.Actions = slices.Contains(caps, "actions")
	}
	return p
}
