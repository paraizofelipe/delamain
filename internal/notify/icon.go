package notify

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed delamain.png
var logo []byte

// writeIcon puts the logo in <stateDir>/notify/delamain.png, rewriting it only when it differs,
// and returns its path for the notification server to read.
func writeIcon(stateDir string) (string, error) {
	dir := filepath.Join(stateDir, "notify")
	path := filepath.Join(dir, "delamain.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, logo) { //nolint:gosec // path inside the state dir
		return path, nil
	}
	// The notification server and terminal-notifier read the icon: it is a public logo.
	if err := os.WriteFile(path, logo, 0o644); err != nil { //nolint:gosec // see above
		return "", err
	}
	return path, os.Chmod(path, 0o644) //nolint:gosec // see above; independent of the umask
}
