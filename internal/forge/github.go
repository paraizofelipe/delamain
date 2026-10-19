package forge

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	gh "github.com/google/go-github/v92/github"
)

type github struct {
	repo        Repo
	owner, name string
	api         *gh.Client
}

func newGitHub(repo Repo, token string, hc *http.Client, o options) (Forge, error) {
	opts := []gh.ClientOptionsFunc{gh.WithHTTPClient(hc), gh.WithAuthToken(token), gh.WithUserAgent(o.userAgent)}
	switch {
	case o.baseURL != "":
		opts = append(opts, gh.WithURLs(&o.baseURL, &o.baseURL))
	case !strings.EqualFold(repo.Host, "github.com"):
		base := "https://" + repo.Host + "/"
		opts = append(opts, gh.WithEnterpriseURLs(base, base)) // appends api/v3/ and api/uploads/
	}
	api, err := gh.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(repo.Path, "/")
	return github{repo: repo, owner: owner, name: name, api: api}, nil
}

func (g github) Repo() Repo { return g.repo }

func ghIssue(v *gh.Issue) Issue {
	is := Issue{Number: v.GetNumber(), Title: v.GetTitle(), Body: v.GetBody(), URL: v.GetHTMLURL(), Open: v.GetState() == "open"}
	for _, l := range v.Labels {
		is.Labels = append(is.Labels, l.GetName())
	}
	return is
}

func (g github) Issue(ctx context.Context, n int) (Issue, error) {
	v, _, err := g.api.Issues.Get(ctx, g.owner, g.name, n)
	if err != nil {
		return Issue{}, err
	}
	return ghIssue(v), nil
}

// ListReady drops pull requests: the issues endpoint lists them too.
func (g github) ListReady(ctx context.Context, label string) ([]Issue, error) {
	vs, _, err := g.api.Issues.ListByRepo(ctx, g.owner, g.name, &gh.IssueListByRepoOptions{
		State: "open", Labels: []string{label}, Sort: "created", Direction: "asc",
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		if !v.IsPullRequest() {
			is = append(is, ghIssue(v))
		}
	}
	return oldestFirst(is), nil
}

// OpenPR filters by owner:branch; the API ignores a head without the owner.
func (g github) OpenPR(ctx context.Context, head string) (*PR, error) {
	prs, _, err := g.api.PullRequests.List(ctx, g.owner, g.name, &gh.PullRequestListOptions{State: "open", Head: g.owner + ":" + head})
	if err != nil || len(prs) == 0 {
		return nil, err
	}
	return &PR{Number: prs[0].GetNumber(), URL: prs[0].GetHTMLURL(), Draft: prs[0].GetDraft()}, nil
}

func (g github) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	v, _, err := g.api.PullRequests.Create(ctx, g.owner, g.name, gh.CreatePullRequest{
		Title: &o.Title, Head: o.Head, Base: o.Base, Body: &o.Body, Draft: &o.Draft,
	})
	if err != nil {
		return PR{}, err
	}
	return PR{Number: v.GetNumber(), URL: v.GetHTMLURL(), Draft: v.GetDraft()}, nil
}

func (g github) Comment(ctx context.Context, n int, body string) error {
	_, _, err := g.api.Issues.CreateComment(ctx, g.owner, g.name, n, gh.IssueCommentRequest{Body: body})
	return err
}

// Relabel creates missing labels first: GitHub rejects unknown labels on edit. Existing labels
// keep the user's color and description.
func (g github) Relabel(ctx context.Context, n int, add, remove []string) error {
	color, desc := "5319e7", "delamain"
	for _, l := range append(append([]string{}, add...), remove...) {
		_, _, err := g.api.Issues.CreateLabel(ctx, g.owner, g.name, gh.CreateIssueLabelRequest{Name: l, Color: &color, Description: &desc})
		if err != nil && !alreadyExists(err) {
			return err
		}
	}
	if len(add) > 0 {
		if _, _, err := g.api.Issues.AddLabelsToIssue(ctx, g.owner, g.name, n, add); err != nil {
			return err
		}
	}
	for _, l := range remove {
		// A 404 means the issue did not carry the label: already the desired state.
		resp, err := g.api.Issues.RemoveLabelForIssue(ctx, g.owner, g.name, n, url.PathEscape(l))
		if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
			return err
		}
	}
	return nil
}

func alreadyExists(err error) bool {
	var e *gh.ErrorResponse
	if !errors.As(err, &e) || e.Response == nil || e.Response.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	return slices.ContainsFunc(e.Errors, func(x gh.Error) bool { return x.Code == "already_exists" })
}
