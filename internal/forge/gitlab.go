package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gogitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

type gitlabClient struct {
	caller
	repo Repo
	api  *gogitlab.Client
}

// opened is GitLab's state of an open issue or MR.
const opened = "opened"

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
	return &gitlabClient{caller: caller{GitLab, token, o.backoff}, repo: repo, api: api}, nil
}

// do runs one API call with retries on gateway and network errors. client-go's own retries
// are off so both forges follow the same rules.
func (g *gitlabClient) do(ctx context.Context, op string, call func() (*gogitlab.Response, error)) error {
	return g.retry(ctx, func() (bool, time.Duration, error) {
		resp, err := call()
		if err == nil {
			return false, 0, nil
		}
		status, msg := 0, ""
		if resp != nil {
			status = resp.StatusCode
		}
		if er, ok := errors.AsType[*gogitlab.ErrorResponse](err); ok {
			msg = er.Message
		} else if errors.Is(err, gogitlab.ErrNotFound) {
			msg = "Not Found"
		}
		return transient(ctx, status, err), 0, g.fail(op, status, msg, err)
	})
}

func (g *gitlabClient) Repo() Repo { return g.repo }

func fromGitLab(v *gogitlab.Issue) Issue {
	return Issue{Number: int(v.IID), Title: v.Title, Body: v.Description, URL: v.WebURL, Labels: v.Labels, Open: v.State == opened}
}

func (g *gitlabClient) Issue(ctx context.Context, n int) (Issue, error) {
	var v *gogitlab.Issue
	err := g.do(ctx, "issue", func() (resp *gogitlab.Response, err error) {
		v, resp, err = g.api.Issues.GetIssue(g.repo.Path, int64(n), gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return Issue{}, err
	}
	return fromGitLab(v), nil
}

func (g *gitlabClient) ListReady(ctx context.Context, label string) ([]Issue, error) {
	var vs []*gogitlab.Issue
	state := opened
	err := g.do(ctx, "list ready", func() (resp *gogitlab.Response, err error) {
		vs, resp, err = g.api.Issues.ListProjectIssues(g.repo.Path, &gogitlab.ListProjectIssuesOptions{
			State: &state, Labels: &gogitlab.LabelOptions{label}, ListOptions: gogitlab.ListOptions{PerPage: 100},
		}, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		is = append(is, fromGitLab(v))
	}
	return oldestFirst(is), nil
}

func (g *gitlabClient) OpenPR(ctx context.Context, head string) (*PR, error) {
	var mrs []*gogitlab.BasicMergeRequest
	state := opened
	err := g.do(ctx, "open pr", func() (resp *gogitlab.Response, err error) {
		mrs, resp, err = g.api.MergeRequests.ListProjectMergeRequests(g.repo.Path, &gogitlab.ListProjectMergeRequestsOptions{
			State: &state, SourceBranch: &head,
		}, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil || len(mrs) == 0 {
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
	var v *gogitlab.MergeRequest
	tries := 0
	err := g.do(ctx, "create pr", func() (resp *gogitlab.Response, err error) {
		tries++
		v, resp, err = g.api.MergeRequests.CreateMergeRequest(g.repo.Path, &gogitlab.CreateMergeRequestOptions{
			Title: &title, Description: &o.Body, SourceBranch: &o.Head, TargetBranch: &o.Base,
		}, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return createdAnyway(ctx, g, o.Head, tries, http.StatusConflict, err)
	}
	return PR{Number: int(v.IID), URL: v.WebURL, Draft: v.Draft}, nil
}

// Comment is retried like any call: a retry after a lost answer may post the comment twice,
// which is harmless next to losing the run's status comment.
func (g *gitlabClient) Comment(ctx context.Context, n int, body string) error {
	return g.do(ctx, "comment", func() (*gogitlab.Response, error) {
		_, resp, err := g.api.Notes.CreateIssueNote(g.repo.Path, int64(n), &gogitlab.CreateIssueNoteOptions{Body: &body}, gogitlab.WithContext(ctx))
		return resp, err
	})
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
	return g.do(ctx, "relabel", func() (*gogitlab.Response, error) {
		_, resp, err := g.api.Issues.UpdateIssue(g.repo.Path, int64(n), opt, gogitlab.WithContext(ctx))
		return resp, err
	})
}

func (g *gitlabClient) whoami(ctx context.Context) (string, error) {
	var u *gogitlab.User
	err := g.do(ctx, "whoami", func() (resp *gogitlab.Response, err error) {
		u, resp, err = g.api.Users.CurrentUser(gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

func (g *gitlabClient) PRView(ctx context.Context, n int) (PRDetail, error) {
	var v *gogitlab.MergeRequest
	err := g.do(ctx, "pr view", func() (resp *gogitlab.Response, err error) {
		v, resp, err = g.api.MergeRequests.GetMergeRequest(g.repo.Path, int64(n), nil, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return PRDetail{}, err
	}
	pr := fromGitLabMR(&v.BasicMergeRequest)
	pr.BaseSHA, pr.HeadSHA, pr.StartSHA = v.DiffRefs.BaseSha, v.DiffRefs.HeadSha, v.DiffRefs.StartSha
	return pr, nil
}

// fromGitLabMR maps what every MR payload carries; the SHAs need diff_refs, which only the
// single MR endpoint returns.
func fromGitLabMR(v *gogitlab.BasicMergeRequest) PRDetail {
	pr := PRDetail{
		Number: int(v.IID), URL: v.WebURL, Title: v.Title, Body: v.Description, Draft: v.Draft, Open: v.State == opened,
		Fork: v.SourceProjectID != v.TargetProjectID, BaseRef: v.TargetBranch, HeadRef: v.SourceBranch,
	}
	if v.Author != nil {
		pr.Author = v.Author.Username
	}
	return pr
}

// ListPRs sees only the first 100 labeled MRs. The listing has no diff_refs: HeadSHA is the
// MR's sha and BaseSHA/StartSHA stay empty; PRView has them all.
func (g *gitlabClient) ListPRs(ctx context.Context, label string) ([]PRDetail, error) {
	var mrs []*gogitlab.BasicMergeRequest
	state := opened
	err := g.do(ctx, "list prs", func() (resp *gogitlab.Response, err error) {
		mrs, resp, err = g.api.MergeRequests.ListProjectMergeRequests(g.repo.Path, &gogitlab.ListProjectMergeRequestsOptions{
			State: &state, Labels: &gogitlab.LabelOptions{label}, ListOptions: gogitlab.ListOptions{PerPage: 100},
		}, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	prs := make([]PRDetail, 0, len(mrs))
	for _, v := range mrs {
		pr := fromGitLabMR(v)
		pr.HeadSHA = v.SHA
		prs = append(prs, pr)
	}
	return oldestPRsFirst(prs), nil
}

// LinkedIssues falls back to the closing keywords of the description when GitLab knows none.
func (g *gitlabClient) LinkedIssues(ctx context.Context, n int) ([]Issue, error) {
	var vs []*gogitlab.Issue
	err := g.do(ctx, "linked issues", func() (resp *gogitlab.Response, err error) {
		vs, resp, err = g.api.MergeRequests.GetIssuesClosedOnMerge(g.repo.Path, int64(n), nil, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	var ns []int
	for _, v := range vs {
		// An issue of another project would be read here under the same IID.
		if v.References != nil && strings.EqualFold(v.References.Full, fmt.Sprintf("%s#%d", g.repo.Path, v.IID)) {
			ns = append(ns, int(v.IID))
		}
	}
	if len(ns) == 0 {
		pr, err := g.PRView(ctx, n)
		if err != nil {
			return nil, err
		}
		ns = closedInBody(pr.Body)
	}
	return readIssues(ctx, g, ns)
}

// Threads reads the first 100 discussions without GitLab's system notes, dropping discussions
// made only of them. Path and line come from the first remaining note, Resolved from the first
// resolvable one.
func (g *gitlabClient) Threads(ctx context.Context, n int) ([]Thread, error) {
	var ds []*gogitlab.Discussion
	err := g.do(ctx, "threads", func() (resp *gogitlab.Response, err error) {
		ds, resp, err = g.api.Discussions.ListMergeRequestDiscussions(g.repo.Path, int64(n), &gogitlab.ListMergeRequestDiscussionsOptions{
			ListOptions: gogitlab.ListOptions{PerPage: 100},
		}, gogitlab.WithContext(ctx))
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	ts := make([]Thread, 0, len(ds))
	for _, d := range ds {
		t, resolvable := Thread{ID: d.ID}, false
		for _, v := range d.Notes {
			if v.System {
				continue
			}
			if len(t.Comments) == 0 && v.Position != nil {
				t.Path, t.Line = v.Position.NewPath, int(v.Position.NewLine)
			}
			if !resolvable && v.Resolvable {
				resolvable, t.Resolved = true, v.Resolved
			}
			c := Comment{ID: strconv.FormatInt(v.ID, 10), Author: v.Author.Username, Body: v.Body}
			if v.CreatedAt != nil {
				c.CreatedAt = *v.CreatedAt
			}
			t.Comments = append(t.Comments, c)
		}
		if len(t.Comments) > 0 {
			ts = append(ts, t)
		}
	}
	return byFirstComment(ts), nil
}

func (g *gitlabClient) PostReview(ctx context.Context, n int, o ReviewOptions) error {
	return errors.New("gitlab post review: not implemented")
}
