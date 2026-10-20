package notify

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/paraizo/delamain/internal/config"
)

const notificationsName = "org.freedesktop.Notifications"

var (
	errNoServer = errors.New("no desktop notification server")
	errNoMacOS  = errors.New("neither terminal-notifier nor osascript found")
)

// resolve picks the backend: macos on darwin (auto) or when asked for, dbus when the session bus
// has a notification server, none otherwise.
func resolve(name string, s sys) (string, bus, error) {
	switch name {
	case config.NotifyNone:
		return config.NotifyNone, nil, nil
	case config.NotifyMacOS:
		return macOS(s)
	case config.NotifyAuto:
		if s.goos == "darwin" {
			return macOS(s)
		}
	}
	b, err := sessionBus(s)
	if err != nil {
		return config.NotifyNone, nil, err
	}
	return config.NotifyDBus, b, nil
}

// macOS needs a program to post with; without one (macos asked for outside a Mac) there is none.
func macOS(s sys) (string, bus, error) {
	for _, p := range []string{"terminal-notifier", "osascript"} {
		if _, err := s.lookPath(p); err == nil {
			return config.NotifyMacOS, nil, nil
		}
	}
	return config.NotifyNone, nil, errNoMacOS
}

// sessionBus connects to the session bus and checks a notification server owns its name.
func sessionBus(s sys) (bus, error) {
	addr := s.getenv("DBUS_SESSION_BUS_ADDRESS")
	if addr == "" {
		dir := s.getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return nil, errNoServer
		}
		addr = "unix:path=" + filepath.Join(dir, "bus")
	}
	// A broker that accepts but never answers must not hold the run that creates the notifier.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	b, err := s.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	owned, err := b.NameHasOwner(ctx, notificationsName)
	if err != nil || !owned {
		_ = b.Close()
		return nil, errors.Join(errNoServer, err)
	}
	return b, nil
}
