package cli

import (
	"context"
	"fmt"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/doctor"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// forgeLookup is where forgeFor reads the forge token; tests replace it.
var forgeLookup forge.TokenLookup = forge.OSLookup{}

func userAgent() forge.Option { return forge.WithUserAgent("delamain/" + Version) }

// forgeFor picks the forge of the repository containing dir. A repository without origin has
// none (work stays local) unless forge.kind names one explicitly, which is then an error. A
// forge without a token is an error too: nothing can be opened on it.
func forgeFor(ctx context.Context, cfg config.Config, dir string) (forge.Forge, error) {
	repo, ok, err := forgeRepo(ctx, cfg, dir)
	if err != nil || !ok {
		return nil, err
	}
	token, _, err := forgeToken(ctx, repo)
	if err != nil {
		return nil, err
	}
	return forge.New(repo, token, userAgent())
}

// forgeAuth is doctor's ForgeAuth: who the token of dir's forge belongs to, via GET /user.
func forgeAuth(ctx context.Context, cfg config.Config, dir string) (doctor.ForgeIdentity, error) {
	repo, ok, err := forgeRepo(ctx, cfg, dir)
	if err != nil || !ok {
		return doctor.ForgeIdentity{}, err
	}
	id := doctor.ForgeIdentity{Kind: repo.Kind, Host: repo.Host}
	token, source, err := forgeToken(ctx, repo)
	if err != nil {
		return id, err
	}
	id.Source = source
	id.Login, err = forge.Whoami(ctx, repo, token, userAgent())
	return id, err
}

// forgeRepo finds the forge of the repository containing dir; ok is false when work stays local.
func forgeRepo(ctx context.Context, cfg config.Config, dir string) (repo forge.Repo, ok bool, err error) {
	if cfg.Forge.Kind == config.ForgeNone {
		return forge.Repo{}, false, nil
	}
	g := gitx.New()
	// The repository, not the checkout dir happens to be: a command called from inside a worktree
	// answers for the same forge as one called from the repository itself.
	top, err := g.MainWorktree(ctx, dir)
	if err != nil {
		return forge.Repo{}, false, fmt.Errorf("repository: %w", err)
	}
	if !g.HasRemote(ctx, top, "origin") {
		if cfg.Forge.Kind != config.ForgeAuto {
			return forge.Repo{}, false, fmt.Errorf("forge.kind=%s, but %s has no origin remote", cfg.Forge.Kind, top)
		}
		return forge.Repo{}, false, nil
	}
	u, err := g.RemoteURL(ctx, top, "origin")
	if err != nil {
		return forge.Repo{}, false, err
	}
	repo, err = forge.Detect(u, cfg.Forge.Kind)
	if err != nil {
		return forge.Repo{}, false, err
	}
	return repo, true, nil
}

// forgeToken resolves repo's token; without one, the error says how to provide it.
func forgeToken(ctx context.Context, repo forge.Repo) (token, source string, err error) {
	token, source, err = forge.Token(ctx, repo.Kind, repo.Host, forgeLookup)
	if err != nil {
		return "", "", fmt.Errorf("%w; %s", err, forge.TokenHint(repo.Kind, repo.Host))
	}
	return token, source, nil
}
