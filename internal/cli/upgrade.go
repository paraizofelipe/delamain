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
		Short: "Atualiza o delamain para a última release publicada",
		Long: `Compara a versão instalada com a última release do GitHub e, se forem diferentes, baixa o
binário desta plataforma, confere o sha256 com checksums.txt e substitui o executável em uso
(normalmente ~/.local/bin/delamain). GITHUB_TOKEN, se definido, evita o rate limit da API.`,
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
				_, err := fmt.Fprintf(out, "já está na última versão (%s)\n", Version)
				return err
			}
			if check {
				_, err := fmt.Fprintf(out, "nova versão disponível: %s → %s (rode delamain upgrade)\n", Version, rel.Tag)
				return err
			}
			if Version == "dev" && !force {
				return errors.New("este é um build do código-fonte (dev); use --force para trocá-lo pela release " + rel.Tag)
			}
			target, err := executable()
			if err != nil {
				return err
			}
			if target, err = filepath.EvalSymlinks(target); err != nil {
				return err
			}
			if err := u.Install(ctx, rel, target); err != nil {
				return fmt.Errorf("upgrade para %s: %w", rel.Tag, err)
			}
			_, err = fmt.Fprintf(out, "%s → %s (%s)\n", Version, rel.Tag, target)
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "só informa se há versão nova, sem trocar o binário")
	cmd.Flags().BoolVar(&force, "force", false, "troca também um build dev (compilado do código-fonte)")
	return cmd
}
