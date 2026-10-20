package notify

import (
	"context"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// clickTargets are what a click may open: https URLs of the forges and files in the state dir.
type clickTargets struct {
	hosts    []string
	stateDir string
}

func (c clickTargets) allowed(target string) bool {
	return forgeURL(target, c.hosts) || c.inStateDir(target)
}

// inStateDir reports whether target is an existing absolute path that, symlinks and ".." resolved,
// stays inside the state dir.
func (c clickTargets) inStateDir(target string) bool {
	if !filepath.IsAbs(target) || c.stateDir == "" {
		return false
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(c.stateDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// xdgOpen opens a target with the desktop's default application, the target as its only argument.
func xdgOpen(ctx context.Context, target string) error {
	return exec.CommandContext(ctx, "xdg-open", target).Run() //nolint:gosec // target validated by clickTargets
}

// forgeURL reports whether target is an https URL on one of the forge hosts: the only URLs a
// click may open.
func forgeURL(target string, hosts []string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	return slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, u.Host) })
}
