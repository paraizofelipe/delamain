package cli

import (
	"context"
	"fmt"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
)

// tokenLookup resolves forge tokens; the CLI fallback runs at most once per host and process.
var tokenLookup forge.TokenLookup = forge.CachedLookup(forge.OSLookup{})

// forgeOptions are extra options for every forge client; tests point them at stub servers.
var forgeOptions []forge.Option

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
	token, _, err := forge.Token(ctx, repo.Kind, repo.Host, tokenLookup)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, forge.TokenHint(repo.Kind, repo.Host))
	}
	return forge.New(repo, token, clientOptions()...)
}

// forgeRepos lists the repositories doctor checks: daemon.repos, or the current one.
func forgeRepos(ctx context.Context, cfg config.Config) ([]forge.Repo, error) {
	if cfg.Forge.Kind == config.ForgeNone {
		return nil, nil
	}
	dirs := cfg.Daemon.Repos
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	g := gitx.New()
	var repos []forge.Repo
	for _, dir := range dirs {
		top, err := g.Toplevel(ctx, dir)
		if err != nil {
			if len(cfg.Daemon.Repos) == 0 {
				continue // doctor run outside a repository
			}
			return nil, fmt.Errorf("repository: %w", err)
		}
		if !g.HasRemote(ctx, top, "origin") {
			continue
		}
		u, err := g.RemoteURL(ctx, top, "origin")
		if err != nil {
			return nil, err
		}
		repo, err := forge.Detect(u, cfg.Forge.Kind)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

// forgeUser says who repo's token authenticates as, and where the token came from.
func forgeUser(ctx context.Context, repo forge.Repo) (login, source string, err error) {
	token, source, err := forge.Token(ctx, repo.Kind, repo.Host, tokenLookup)
	if err != nil {
		return "", "", err
	}
	login, err = forge.CurrentUser(ctx, repo, token, clientOptions()...)
	return login, source, err
}

func clientOptions() []forge.Option {
	return append([]forge.Option{forge.WithUserAgent("delamain/" + Version)}, forgeOptions...)
}
