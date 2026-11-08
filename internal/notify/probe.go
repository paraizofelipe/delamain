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
	// Ntfy describes the ntfy push when notify.ntfy is enabled; nil otherwise.
	Ntfy *NtfyProbe
}

// Diagnose resolves the configured backend, as New does, and describes it; with ntfy on, it
// also asks the ntfy server whether it is healthy.
func Diagnose(ctx context.Context, cfg config.Notify) Probe {
	return diagnoseAll(ctx, cfg, realSys())
}

func diagnoseAll(ctx context.Context, cfg config.Notify, s sys) Probe {
	p := diagnose(ctx, cfg.Backend, s)
	p.Ntfy = diagnoseNtfy(ctx, cfg.Ntfy, s)
	return p
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
