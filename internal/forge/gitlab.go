package forge

import (
	"context"
	"errors"
	"fmt"

	gogitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

type gitlabClient struct {
	repo Repo
	api  *gogitlab.Client
}

// newGitLab talks to https://<host>/api/v4 on gitlab.com and self-hosted instances alike.
func newGitLab(repo Repo, token string, o options) (*gitlabClient, error) {
	base := "https://" + repo.Host
	if o.baseURL != "" {
		base = o.baseURL
	}
	api, err := gogitlab.NewClient(token,
		gogitlab.WithBaseURL(base),
		gogitlab.WithHTTPClient(o.httpClient),
		gogitlab.WithUserAgent(o.userAgent),
		gogitlab.WithoutRetries(),
	)
	if err != nil {
		return nil, fmt.Errorf("gitlab client: %w", err)
	}
	return &gitlabClient{repo: repo, api: api}, nil
}

// check turns a client-go failure into an error naming the operation and the HTTP status.
func (g *gitlabClient) check(op string, resp *gogitlab.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.StatusCode >= 400 {
		msg := ""
		if er, ok := errors.AsType[*gogitlab.ErrorResponse](err); ok {
			msg = er.Message
		} else if errors.Is(err, gogitlab.ErrNotFound) {
			msg = "Not Found"
		}
		return &APIError{Forge: GitLab, Op: op, Status: resp.StatusCode, Message: msg}
	}
	return fmt.Errorf("gitlab %s: %w", op, err)
}

func (g *gitlabClient) Repo() Repo { return g.repo }

func fromGitLab(v *gogitlab.Issue) Issue {
	return Issue{Number: int(v.IID), Title: v.Title, Body: v.Description, URL: v.WebURL, Labels: v.Labels, Open: v.State == "opened"}
}

func (g *gitlabClient) Issue(ctx context.Context, n int) (Issue, error) {
	v, resp, err := g.api.Issues.GetIssue(g.repo.Path, int64(n), gogitlab.WithContext(ctx))
	if err := g.check("issue", resp, err); err != nil {
		return Issue{}, err
	}
	return fromGitLab(v), nil
}

func (g *gitlabClient) ListReady(ctx context.Context, label string) ([]Issue, error) {
	vs, resp, err := g.api.Issues.ListProjectIssues(g.repo.Path, &gogitlab.ListProjectIssuesOptions{
		State: gogitlab.Ptr("opened"), Labels: &gogitlab.LabelOptions{label}, ListOptions: gogitlab.ListOptions{PerPage: 100},
	}, gogitlab.WithContext(ctx))
	if err := g.check("list ready", resp, err); err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		is = append(is, fromGitLab(v))
	}
	return oldestFirst(is), nil
}

func (g *gitlabClient) OpenPR(ctx context.Context, head string) (*PR, error) {
	mrs, resp, err := g.api.MergeRequests.ListProjectMergeRequests(g.repo.Path, &gogitlab.ListProjectMergeRequestsOptions{
		State: gogitlab.Ptr("opened"), SourceBranch: &head,
	}, gogitlab.WithContext(ctx))
	if err := g.check("open pr", resp, err); err != nil || len(mrs) == 0 {
		return nil, err
	}
	return &PR{Number: int(mrs[0].IID), URL: mrs[0].WebURL, Draft: mrs[0].Draft}, nil
}

// CreatePR marks drafts with GitLab's "Draft: " title prefix; the branch is already pushed.
func (g *gitlabClient) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	title := o.Title
	if o.Draft {
		title = "Draft: " + title
	}
	v, resp, err := g.api.MergeRequests.CreateMergeRequest(g.repo.Path, &gogitlab.CreateMergeRequestOptions{
		Title: &title, Description: &o.Body, SourceBranch: &o.Head, TargetBranch: &o.Base,
	}, gogitlab.WithContext(ctx))
	if err := g.check("create pr", resp, err); err != nil {
		return PR{}, err
	}
	return PR{Number: int(v.IID), URL: v.WebURL, Draft: v.Draft}, nil
}

func (g *gitlabClient) Comment(ctx context.Context, n int, body string) error {
	_, resp, err := g.api.Notes.CreateIssueNote(g.repo.Path, int64(n), &gogitlab.CreateIssueNoteOptions{Body: &body}, gogitlab.WithContext(ctx))
	return g.check("comment", resp, err)
}

// Relabel relies on GitLab creating unknown labels when they are assigned.
func (g *gitlabClient) Relabel(ctx context.Context, n int, add, remove []string) error {
	opt := &gogitlab.UpdateIssueOptions{}
	if len(add) > 0 {
		opt.AddLabels = (*gogitlab.LabelOptions)(&add)
	}
	if len(remove) > 0 {
		opt.RemoveLabels = (*gogitlab.LabelOptions)(&remove)
	}
	_, resp, err := g.api.Issues.UpdateIssue(g.repo.Path, int64(n), opt, gogitlab.WithContext(ctx))
	return g.check("relabel", resp, err)
}
