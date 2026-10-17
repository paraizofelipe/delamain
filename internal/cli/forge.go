package cli

import (
	"context"
	"fmt"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// forgeFor picks the forge of the repository containing dir. A repository without origin has
// none (work stays local) unless forge.kind names one explicitly, which is then an error.
func forgeFor(ctx context.Context, cfg config.Config, dir string) (forge.Forge, error) {
	if cfg.Forge.Kind == config.ForgeNone {
		return nil, nil
	}
	g := gitx.New()
	top, err := g.Toplevel(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("repository: %w", err)
	}
	if !g.HasRemote(ctx, top, "origin") {
		if cfg.Forge.Kind != config.ForgeAuto {
			return nil, fmt.Errorf("forge.kind=%s, mas %s não tem remote origin", cfg.Forge.Kind, top)
		}
		return nil, nil
	}
	u, err := g.RemoteURL(ctx, top, "origin")
	if err != nil {
		return nil, err
	}
	repo, err := forge.Detect(u, cfg.Forge.Kind)
	if err != nil {
		return nil, err
	}
	return forge.New(repo, forge.ExecRunner{Binary: forge.Binary(repo.Kind), Dir: top}), nil
}
