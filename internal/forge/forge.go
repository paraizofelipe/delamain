// Package forge opens PRs (GitHub) and MRs (GitLab) and manages issue labels and comments.
// It deliberately has no merge operation: delamain stops at the PR/MR.
//
// It talks HTTP through the official-ish SDKs instead of executing the forge CLIs:
// github.com/google/go-github (maintained by Google, the de facto GitHub client) and
// gitlab.com/gitlab-org/api/client-go (maintained by GitLab itself). Both are typed, cover
// Enterprise and self-hosted base URLs and need nothing installed on the machine.
package forge

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
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
			return "", "", fmt.Errorf("remote sem host de forge: %q", redact(raw, ""))
		}
		host, path = u.Hostname(), u.Path // Hostname drops userinfo and port
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if !strings.Contains(path, "/") {
		return "", "", fmt.Errorf("remote sem caminho owner/repo: %q", redact(raw, ""))
	}
	return host, path, nil
}

// userinfo matches the credentials part of any URL inside a text.
var userinfo = regexp.MustCompile(`(\w+://)[^/@\s"]+@`)

// redact drops URL userinfo and every occurrence of token so credentials never reach logs.
func redact(raw, token string) string {
	if token != "" {
		raw = strings.ReplaceAll(raw, token, "***")
	}
	return userinfo.ReplaceAllString(raw, "$1")
}

func oldestFirst(is []Issue) []Issue {
	slices.SortFunc(is, func(a, b Issue) int { return a.Number - b.Number })
	return is
}
