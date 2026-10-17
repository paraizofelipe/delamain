// Package gitx wraps the few git operations the orchestrator performs itself.
// It never merges, rebases, resets or pushes to a protected branch.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Protected branches are never used as work branches.
var Protected = []string{"main", "master"}

type Git struct{ Binary string }

func New() Git { return Git{Binary: "git"} }

func (g Git) run(ctx context.Context, dir string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, g.Binary, append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Toplevel resolves the repository root containing dir.
func (g Git) Toplevel(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--show-toplevel")
}

// HasRemote reports whether the repository has the named remote.
func (g Git) HasRemote(ctx context.Context, repo, name string) bool {
	_, err := g.run(ctx, repo, "remote", "get-url", name)
	return err == nil
}

// RemoteURL returns the URL of a remote.
func (g Git) RemoteURL(ctx context.Context, repo, name string) (string, error) {
	return g.run(ctx, repo, "remote", "get-url", name)
}

// ResolveBase picks the ref new work branches start from: origin's default branch after a
// fetch when a remote exists, otherwise the repository's current local branch.
func (g Git) ResolveBase(ctx context.Context, repo string) (string, error) {
	if g.HasRemote(ctx, repo, "origin") {
		if _, err := g.run(ctx, repo, "fetch", "--quiet", "origin"); err != nil {
			return "", err
		}
		ref, err := g.run(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
		if err == nil {
			return strings.TrimPrefix(ref, "refs/remotes/"), nil
		}
		for _, b := range Protected {
			if _, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", "origin/"+b); err == nil {
				return "origin/" + b, nil
			}
		}
		return "", errors.New("cannot resolve origin's default branch")
	}
	return g.run(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
}

func (g Git) Head(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "HEAD")
}

func (g Git) CurrentBranch(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// Status returns `git status --porcelain` (empty means clean).
func (g Git) Status(ctx context.Context, dir string) (string, error) {
	return g.run(ctx, dir, "status", "--porcelain")
}

// CommitsSince lists `<sha> <subject>` of commits in base..HEAD, oldest first.
func (g Git) CommitsSince(ctx context.Context, dir, base string) ([]string, error) {
	out, err := g.run(ctx, dir, "log", "--reverse", "--format=%h %s", base+"..HEAD")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// ChangedFiles lists files changed in base...HEAD.
func (g Git) ChangedFiles(ctx context.Context, dir, base string) ([]string, error) {
	out, err := g.run(ctx, dir, "diff", "--name-only", base+"...HEAD")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// BranchExists reports whether a local branch exists.
func (g Git) BranchExists(ctx context.Context, repo, branch string) bool {
	_, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// GuardWorkBranch refuses protected or base branches as the place agents write to.
func GuardWorkBranch(branch, base string) error {
	short := strings.TrimPrefix(base, "origin/")
	for _, p := range append([]string{short}, Protected...) {
		if branch == p {
			return fmt.Errorf("refusing to work on protected branch %q", branch)
		}
	}
	if strings.TrimSpace(branch) == "" || branch == "HEAD" {
		return errors.New("refusing to work on a detached or empty branch")
	}
	return nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a kebab-case ASCII fragment of at most max runes.
func Slug(title string, max int) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(title)) {
		if unicode.Is(unicode.Mn, r) {
			continue // drop accents: "ção" -> "cao"
		}
		b.WriteRune(r)
	}
	s := strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	if len(s) > max {
		cut := s[:max]
		// Prefer a word boundary so branches never end in half a word.
		if s[max] != '-' {
			if i := strings.LastIndexByte(cut, '-'); i > 0 {
				cut = cut[:i]
			}
		}
		s = strings.TrimRight(cut, "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// BranchName builds `fix/<slug>` for bugs and `feat/<slug>` otherwise.
func BranchName(kind, ref, title string) string {
	prefix := "feat"
	if kind == "bug" {
		prefix = "fix"
	}
	id := Slug(ref, 12)
	return prefix + "/" + id + "-" + Slug(title, 40)
}
