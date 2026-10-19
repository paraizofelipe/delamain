package notify

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

//go:embed delamain.png
var logo []byte

// writeIcon puts the logo at <stateDir>/notify/delamain.png, where servers can read it, and
// returns that path. The file is rewritten only when its content differs.
func writeIcon(stateDir string) (string, error) {
	dir := filepath.Join(stateDir, "notify")
	path := filepath.Join(dir, "delamain.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, logo) { //nolint:gosec // inside the state dir
		return path, nil
	}
	// World-readable: the notification server is another process that loads the image.
	if err := os.WriteFile(path, logo, 0o644); err != nil { //nolint:gosec // a public logo, not a secret
		return "", err
	}
	return path, os.Chmod(path, 0o644) //nolint:gosec // see above
}
