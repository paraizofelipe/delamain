package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/upgrade"
)

// Overridden by tests: where releases come from and which binary gets replaced.
var (
	upgradeAPI = upgrade.DefaultAPI
	executable = os.Executable
)

func newUpgradeCmd() *cobra.Command {
	var check, force bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Update delamain to the latest published release",
		Long: `Compares the installed version with the latest GitHub release and, when they differ, downloads the
binary for this platform, verifies its sha256 against checksums.txt and replaces the current executable
(normally ~/.local/bin/delamain). GITHUB_TOKEN, when set, avoids the API rate limit.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
			defer cancel()
			u := upgrade.Upgrader{
				API: upgradeAPI, HTTP: &http.Client{Timeout: 2 * time.Minute}, Token: os.Getenv("GITHUB_TOKEN"),
				OS: runtime.GOOS, Arch: runtime.GOARCH,
			}
			rel, err := u.Latest(ctx)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if rel.Tag == Version {
				_, err := fmt.Fprintf(out, "already on the latest version (%s)\n", Version)
				return err
			}
			if check {
				_, err := fmt.Fprintf(out, "new version available: %s → %s (run delamain upgrade)\n", Version, rel.Tag)
				return err
			}
			if Version == "dev" && !force {
				return errors.New("this is a source build (dev); use --force to replace it with release " + rel.Tag)
			}
			target, err := executable()
			if err != nil {
				return err
			}
			if target, err = filepath.EvalSymlinks(target); err != nil {
				return err
			}
			if err := u.Install(ctx, rel, target); err != nil {
				return fmt.Errorf("upgrade to %s: %w", rel.Tag, err)
			}
			_, err = fmt.Fprintf(out, "%s → %s (%s)\n", Version, rel.Tag, target)
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether a new version is available; do not replace the binary")
	cmd.Flags().BoolVar(&force, "force", false, "also replace a dev build (built from source)")
	return cmd
}
