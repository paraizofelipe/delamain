package notify

import (
	"errors"
	"path/filepath"

	"github.com/paraizo/delamain/internal/config"
)

const notificationsName = "org.freedesktop.Notifications"

var errNoServer = errors.New("no desktop notification server")

// resolve picks the backend: macos on darwin (auto) or when asked for, dbus when the session bus
// has a notification server, none otherwise.
func resolve(name string, s sys) (string, bus, error) {
	switch name {
	case config.NotifyNone:
		return config.NotifyNone, nil, nil
	case config.NotifyMacOS:
		return config.NotifyMacOS, nil, nil
	case config.NotifyAuto:
		if s.goos == "darwin" {
			return config.NotifyMacOS, nil, nil
		}
	}
	b, err := sessionBus(s)
	if err != nil {
		return config.NotifyNone, nil, err
	}
	return config.NotifyDBus, b, nil
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
	b, err := s.dial(addr)
	if err != nil {
		return nil, err
	}
	owned, err := b.NameHasOwner(notificationsName)
	if err != nil || !owned {
		_ = b.Close()
		return nil, errors.Join(errNoServer, err)
	}
	return b, nil
}
