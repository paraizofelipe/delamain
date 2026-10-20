package cli

import (
	"context"
	"fmt"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// forgeLookup is where forgeFor reads the forge token; tests replace it.
var forgeLookup forge.TokenLookup = forge.OSLookup{}

// forgeFor picks the forge of the repository containing dir. A repository without origin has
// none (work stays local) unless forge.kind names one explicitly, which is then an error. A
// forge without a token is an error too: nothing can be opened on it.
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
			return nil, fmt.Errorf("forge.kind=%s, but %s has no origin remote", cfg.Forge.Kind, top)
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
	token, _, err := forge.Token(ctx, repo.Kind, repo.Host, forgeLookup)
	if err != nil {
		return nil, fmt.Errorf("%w; %s", err, forge.TokenHint(repo.Kind, repo.Host))
	}
	return forge.New(repo, token, forge.WithUserAgent("delamain/"+Version))
}
