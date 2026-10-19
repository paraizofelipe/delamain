package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// validTarget accepts only what a toast of ours may open: an https URL on the forge host of the
// toast, or an existing absolute path inside the state dir. Anything else came from somewhere else.
func validTarget(target, forgeHost, stateDir string) error {
	if filepath.IsAbs(target) {
		return insideStateDir(target, stateDir)
	}
	u, err := url.Parse(target)
	switch {
	case err != nil:
		return err
	case u.Scheme != "https" || u.Host == "" || u.User != nil:
		return errors.New("only https URLs without credentials are opened")
	case forgeHost == "" || !strings.EqualFold(u.Hostname(), forgeHost):
		return fmt.Errorf("host %q is not the forge of the run (%q)", u.Hostname(), forgeHost)
	}
	return nil
}

func insideStateDir(path, stateDir string) error {
	root, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path) // the file must exist; links are followed
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside the state dir %s", path, stateDir)
	}
	return nil
}

// xdgOpen opens target with the desktop's default app, without a shell and as a single argument.
func xdgOpen(ctx context.Context, target string) error {
	return exec.CommandContext(ctx, "xdg-open", target).Run() //nolint:gosec // target passed validTarget
}
