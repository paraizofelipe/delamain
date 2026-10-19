package forge

import (
	"context"
	"net/http"
	"time"

	gl "gitlab.com/gitlab-org/api/client-go/v3"
)

type gitlab struct {
	repo  Repo
	api   *gl.Client
	scrub func(string) string // removes the token from error text
}

func newGitLab(repo Repo, token string, hc *http.Client, o options) (Forge, error) {
	base := o.baseURL
	if base == "" {
		base = "https://" + repo.Host // the client appends api/v4/
	}
	hc.Timeout = o.timeout // client-go retries above the HTTP client: this bounds each attempt
	backoff := func(_, _ time.Duration, retry int, _ *http.Response) time.Duration { return o.backoff << retry }
	api, err := gl.NewClient(token, gl.WithBaseURL(base), gl.WithHTTPClient(hc), gl.WithUserAgent(o.userAgent),
		gl.WithCustomRetryMax(attempts-1), gl.WithCustomBackoff(backoff), gl.WithCustomRetry(gitlabRetry))
	if err != nil {
		return nil, err
	}
	return gitlab{repo: repo, api: api, scrub: scrubber(token)}, nil
}

func (g gitlab) Repo() Repo { return g.repo }

func (g gitlab) fail(op string, err error) error { return apiError(GitLab, op, g.scrub, err) }

func (g gitlab) user(ctx context.Context) (string, error) {
	u, _, err := g.api.Users.CurrentUser(gl.WithContext(ctx))
	if err != nil {
		return "", g.fail("user", err)
	}
	if u == nil {
		return "", g.fail("user", errEmpty)
	}
	return u.Username, nil
}

func glIssue(v *gl.Issue) Issue {
	return Issue{Number: int(v.IID), Title: v.Title, Body: v.Description, URL: v.WebURL, Labels: v.Labels, Open: v.State == "opened"}
}

// Issue identifies the project by its full path; the client URL-encodes it.
func (g gitlab) Issue(ctx context.Context, n int) (Issue, error) {
	v, _, err := g.api.Issues.GetIssue(g.repo.Path, int64(n), gl.WithContext(ctx))
	if err != nil {
		return Issue{}, g.fail("issue", err)
	}
	if v == nil {
		return Issue{}, g.fail("issue", errEmpty)
	}
	return glIssue(v), nil
}

func (g gitlab) ListReady(ctx context.Context, label string) ([]Issue, error) {
	vs, _, err := g.api.Issues.ListProjectIssues(g.repo.Path, &gl.ListProjectIssuesOptions{
		State: gl.Ptr("opened"), Labels: &gl.LabelOptions{label}, OrderBy: gl.Ptr("created_at"), Sort: gl.Ptr("asc"),
		ListOptions: gl.ListOptions{PerPage: 100},
	}, gl.WithContext(ctx))
	if err != nil {
		return nil, g.fail("listready", err)
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		is = append(is, glIssue(v))
	}
	return oldestFirst(is), nil
}

func (g gitlab) OpenPR(ctx context.Context, head string) (*PR, error) {
	mrs, _, err := g.api.MergeRequests.ListProjectMergeRequests(g.repo.Path, &gl.ListProjectMergeRequestsOptions{
		SourceBranch: &head, State: gl.Ptr("opened"),
	}, gl.WithContext(ctx))
	if err != nil || len(mrs) == 0 {
		return nil, g.fail("openpr", err)
	}
	return &PR{Number: int(mrs[0].IID), URL: mrs[0].WebURL, Draft: mrs[0].Draft}, nil
}

// CreatePR marks drafts by title: the MR API has no draft field.
func (g gitlab) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	title := o.Title
	if o.Draft {
		title = "Draft: " + title
	}
	v, _, err := g.api.MergeRequests.CreateMergeRequest(g.repo.Path, &gl.CreateMergeRequestOptions{
		Title: &title, Description: &o.Body, SourceBranch: &o.Head, TargetBranch: &o.Base,
	}, gl.WithContext(ctx))
	if err != nil {
		return PR{}, g.fail("createpr", err)
	}
	return PR{Number: int(v.IID), URL: v.WebURL, Draft: v.Draft}, nil
}

func (g gitlab) Comment(ctx context.Context, n int, body string) error {
	_, _, err := g.api.Notes.CreateIssueNote(g.repo.Path, int64(n), &gl.CreateIssueNoteOptions{Body: &body}, gl.WithContext(ctx))
	return g.fail("comment", err)
}

// Relabel relies on GitLab creating unknown labels when they are assigned.
func (g gitlab) Relabel(ctx context.Context, n int, add, remove []string) error {
	opt := &gl.UpdateIssueOptions{}
	if len(add) > 0 {
		opt.AddLabels = (*gl.LabelOptions)(&add)
	}
	if len(remove) > 0 {
		opt.RemoveLabels = (*gl.LabelOptions)(&remove)
	}
	_, _, err := g.api.Issues.UpdateIssue(g.repo.Path, int64(n), opt, gl.WithContext(ctx))
	return g.fail("relabel", err)
}
