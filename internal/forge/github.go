package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
)

type githubClient struct {
	repo        Repo
	owner, name string
	api         *gogithub.Client
}

// newGitHub talks to api.github.com, or to https://<host>/api/v3/ on Enterprise.
func newGitHub(repo Repo, token string, o options) (*githubClient, error) {
	owner, name, _ := strings.Cut(repo.Path, "/")
	copts := []gogithub.ClientOptionsFunc{
		gogithub.WithHTTPClient(o.httpClient),
		gogithub.WithAuthToken(token),
		gogithub.WithUserAgent(o.userAgent),
	}
	switch {
	case o.baseURL != "":
		copts = append(copts, gogithub.WithURLs(&o.baseURL, &o.baseURL))
	case repo.Host != "github.com":
		base := "https://" + repo.Host + "/"
		copts = append(copts, gogithub.WithEnterpriseURLs(base, base))
	}
	api, err := gogithub.NewClient(copts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &githubClient{repo: repo, owner: owner, name: name, api: api}, nil
}

// check turns a go-github failure into an error naming the operation and the HTTP status.
func (g *githubClient) check(op string, resp *gogithub.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.StatusCode >= 400 {
		msg := ""
		if er, ok := errors.AsType[*gogithub.ErrorResponse](err); ok {
			msg = er.Message
		}
		return &APIError{Forge: GitHub, Op: op, Status: resp.StatusCode, Message: msg}
	}
	return fmt.Errorf("github %s: %w", op, err)
}

func (g *githubClient) Repo() Repo { return g.repo }

func toIssue(v *gogithub.Issue) Issue {
	is := Issue{Number: v.GetNumber(), Title: v.GetTitle(), Body: v.GetBody(), URL: v.GetHTMLURL(), Open: v.GetState() == "open"}
	for _, l := range v.Labels {
		is.Labels = append(is.Labels, l.GetName())
	}
	return is
}

func (g *githubClient) Issue(ctx context.Context, n int) (Issue, error) {
	v, resp, err := g.api.Issues.Get(ctx, g.owner, g.name, n)
	if err := g.check("issue", resp, err); err != nil {
		return Issue{}, err
	}
	return toIssue(v), nil
}

// ListReady drops pull requests: GitHub's issues endpoint returns them too.
func (g *githubClient) ListReady(ctx context.Context, label string) ([]Issue, error) {
	vs, resp, err := g.api.Issues.ListByRepo(ctx, g.owner, g.name, &gogithub.IssueListByRepoOptions{
		State: "open", Labels: []string{label}, ListOptions: gogithub.ListOptions{PerPage: 100},
	})
	if err := g.check("list ready", resp, err); err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		if !v.IsPullRequest() {
			is = append(is, toIssue(v))
		}
	}
	return oldestFirst(is), nil
}

func (g *githubClient) OpenPR(ctx context.Context, head string) (*PR, error) {
	prs, resp, err := g.api.PullRequests.List(ctx, g.owner, g.name, &gogithub.PullRequestListOptions{State: "open", Head: g.owner + ":" + head})
	if err := g.check("open pr", resp, err); err != nil || len(prs) == 0 {
		return nil, err
	}
	return &PR{Number: prs[0].GetNumber(), URL: prs[0].GetHTMLURL(), Draft: prs[0].GetDraft()}, nil
}

func (g *githubClient) CreatePR(context.Context, PROptions) (PR, error) {
	return PR{}, errors.New("github create pr: not implemented")
}

func (g *githubClient) Comment(context.Context, int, string) error {
	return errors.New("github comment: not implemented")
}

func (g *githubClient) Relabel(context.Context, int, []string, []string) error {
	return errors.New("github relabel: not implemented")
}
