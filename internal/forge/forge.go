// Package forge opens PRs (GitHub, via gh) and MRs (GitLab, via glab) and manages issue
// labels and comments. It deliberately has no merge operation: delamain stops at the PR/MR.
package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// Repo identifies a project on a forge. It never carries credentials.
type Repo struct {
	Kind Kind
	Host string
	Path string // owner/repo or group/subgroup/repo
}

func (r Repo) String() string { return r.Host + "/" + r.Path }

type Issue struct {
	Number int
	Title  string
	Body   string
	URL    string
	Labels []string
	Open   bool
}

// PR is a GitHub pull request or a GitLab merge request.
type PR struct {
	Number int
	URL    string
	Draft  bool
}

type PROptions struct {
	Head, Base  string
	Title, Body string
	Draft       bool
}

// Forge is everything delamain does on a forge. There is intentionally no Merge.
type Forge interface {
	Repo() Repo
	Issue(ctx context.Context, number int) (Issue, error)
	// ListReady returns open issues carrying label, oldest (lowest number) first.
	ListReady(ctx context.Context, label string) ([]Issue, error)
	// OpenPR returns the open PR/MR whose source is head, or nil.
	OpenPR(ctx context.Context, head string) (*PR, error)
	CreatePR(ctx context.Context, o PROptions) (PR, error)
	Comment(ctx context.Context, number int, body string) error
	Relabel(ctx context.Context, number int, add, remove []string) error
}

// Runner executes the forge CLI; stdin carries bodies so they never appear in args or errors.
type Runner interface {
	Run(ctx context.Context, stdin string, args ...string) (string, error)
}

// Binary is the CLI that talks to a forge kind.
func Binary(k Kind) string {
	if k == GitLab {
		return "glab"
	}
	return "gh"
}

// New returns the forge client for repo.
func New(repo Repo, run Runner) Forge {
	if repo.Kind == GitLab {
		return gitlab{repo: repo, run: run}
	}
	return github{repo: repo, run: run}
}

var scpLike = regexp.MustCompile(`^[\w.-]+@([\w.-]+):(.+)$`)

// Detect parses the origin URL and picks the forge: kind "auto" recognizes github.com and hosts
// containing "gitlab"; "github"/"gitlab" force it for self-hosted instances.
func Detect(remoteURL, kind string) (Repo, error) {
	host, path, err := parseRemote(remoteURL)
	if err != nil {
		return Repo{}, err
	}
	repo := Repo{Host: host, Path: path}
	switch kind {
	case string(GitHub):
		repo.Kind = GitHub
	case string(GitLab):
		repo.Kind = GitLab
	case "", "auto":
		switch {
		case host == "github.com":
			repo.Kind = GitHub
		case strings.Contains(host, "gitlab"):
			repo.Kind = GitLab
		default:
			return Repo{}, fmt.Errorf("forge não reconhecido para o host %q; defina forge.kind (github|gitlab|none)", host)
		}
	default:
		return Repo{}, fmt.Errorf("forge.kind %q inválido", kind)
	}
	return repo, nil
}

func parseRemote(raw string) (host, path string, err error) {
	raw = strings.TrimSpace(raw)
	if m := scpLike.FindStringSubmatch(raw); m != nil {
		host, path = m[1], m[2]
	} else {
		u, perr := url.Parse(raw)
		if perr != nil || u.Host == "" {
			return "", "", fmt.Errorf("remote sem host de forge: %q", redact(raw))
		}
		host, path = u.Hostname(), u.Path // Hostname drops userinfo and port
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !strings.Contains(path, "/") {
		return "", "", fmt.Errorf("remote sem caminho owner/repo: %q", redact(raw))
	}
	return host, path, nil
}

// redact drops userinfo so tokens embedded in remote URLs never reach logs.
func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return raw
}

// ExecRunner runs a forge CLI non-interactively. Auth comes from the CLI's own login or env.
type ExecRunner struct {
	Binary string
	Dir    string
}

func (e ExecRunner) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, e.Binary, args...)
	cmd.Dir = e.Dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "NO_PROMPT=1", "NO_COLOR=1", "GH_NO_UPDATE_NOTIFIER=1", "GLAB_CHECK_UPDATE=false")
	if err := cmd.Run(); err != nil {
		label := strings.Join(args[:min(2, len(args))], " ")
		return "", fmt.Errorf("%s %s: %w: %s", e.Binary, label, err, truncate(strings.TrimSpace(errb.String()), 500))
	}
	return out.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// prFromOutput extracts the PR/MR URL the CLIs print on creation.
func prFromOutput(out string, re *regexp.Regexp, draft bool) (PR, error) {
	m := re.FindStringSubmatch(out)
	if m == nil {
		return PR{}, errors.New("a saída não contém a URL do PR/MR criado: " + truncate(strings.TrimSpace(out), 200))
	}
	n, _ := strconv.Atoi(m[2])
	return PR{Number: n, URL: m[1], Draft: draft}, nil
}

func oldestFirst(is []Issue) []Issue {
	slices.SortFunc(is, func(a, b Issue) int { return a.Number - b.Number })
	return is
}
