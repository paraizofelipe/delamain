package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

type githubClient struct {
	caller
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
	return &githubClient{caller: caller{GitHub, token, o.backoff}, repo: repo, owner: owner, name: name, api: api}, nil
}

// do runs one API call with retries, hiding bodies from its error. Besides gateway and network
// errors, GitHub's secondary rate limit is transient: it says how long to wait in Retry-After.
func (g *githubClient) do(ctx context.Context, op string, call func() (*gogithub.Response, error), bodies ...string) error {
	return g.retry(ctx, func() (bool, time.Duration, error) {
		resp, err := call()
		if err == nil {
			return false, 0, nil
		}
		status, msg := 0, ""
		if resp != nil {
			status = resp.StatusCode
		}
		if er, ok := errors.AsType[*gogithub.ErrorResponse](err); ok {
			msg = er.Message
		}
		if rl, ok := errors.AsType[*gogithub.AbuseRateLimitError](err); ok {
			return true, rl.GetRetryAfter(), g.fail(op, status, rl.Message, err, bodies)
		}
		if rl, ok := errors.AsType[*gogithub.RateLimitError](err); ok {
			msg = rl.Message
		}
		return transient(ctx, status, err), 0, g.fail(op, status, msg, err, bodies)
	})
}

func (g *githubClient) Repo() Repo { return g.repo }

func fromGitHub(v *gogithub.Issue) Issue {
	is := Issue{Number: v.GetNumber(), Title: v.GetTitle(), Body: v.GetBody(), URL: v.GetHTMLURL(), Open: v.GetState() == "open"}
	for _, l := range v.Labels {
		is.Labels = append(is.Labels, l.GetName())
	}
	return is
}

func (g *githubClient) Issue(ctx context.Context, n int) (Issue, error) {
	var v *gogithub.Issue
	err := g.do(ctx, "issue", func() (resp *gogithub.Response, err error) {
		v, resp, err = g.api.Issues.Get(ctx, g.owner, g.name, n)
		return resp, err
	})
	if err != nil {
		return Issue{}, err
	}
	return fromGitHub(v), nil
}

// ListReady drops pull requests: GitHub's issues endpoint returns them too.
func (g *githubClient) ListReady(ctx context.Context, label string) ([]Issue, error) {
	var vs []*gogithub.Issue
	err := g.do(ctx, "list ready", func() (resp *gogithub.Response, err error) {
		vs, resp, err = g.api.Issues.ListByRepo(ctx, g.owner, g.name, &gogithub.IssueListByRepoOptions{
			State: "open", Labels: []string{label}, ListOptions: gogithub.ListOptions{PerPage: 100},
		})
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	is := make([]Issue, 0, len(vs))
	for _, v := range vs {
		if !v.IsPullRequest() {
			is = append(is, fromGitHub(v))
		}
	}
	return oldestFirst(is), nil
}

func (g *githubClient) OpenPR(ctx context.Context, head string) (*PR, error) {
	var prs []*gogithub.PullRequest
	err := g.do(ctx, "open pr", func() (resp *gogithub.Response, err error) {
		prs, resp, err = g.api.PullRequests.List(ctx, g.owner, g.name, &gogithub.PullRequestListOptions{State: "open", Head: g.owner + ":" + head})
		return resp, err
	})
	if err != nil || len(prs) == 0 {
		return nil, err
	}
	return &PR{Number: prs[0].GetNumber(), URL: prs[0].GetHTMLURL(), Draft: prs[0].GetDraft()}, nil
}

func (g *githubClient) CreatePR(ctx context.Context, o PROptions) (PR, error) {
	var v *gogithub.PullRequest
	tries := 0
	err := g.do(ctx, "create pr", func() (resp *gogithub.Response, err error) {
		tries++
		v, resp, err = g.api.PullRequests.Create(ctx, g.owner, g.name, gogithub.CreatePullRequest{
			Title: &o.Title, Head: o.Head, Base: o.Base, Body: &o.Body, Draft: &o.Draft,
		})
		return resp, err
	})
	if err != nil {
		return createdAnyway(ctx, g, o.Head, tries, http.StatusUnprocessableEntity, err)
	}
	return PR{Number: v.GetNumber(), URL: v.GetHTMLURL(), Draft: v.GetDraft()}, nil
}

// Comment is retried like any call: a retry after a lost answer may post the comment twice,
// which is harmless next to losing the run's status comment.
func (g *githubClient) Comment(ctx context.Context, n int, body string) error {
	return g.do(ctx, "comment", func() (*gogithub.Response, error) {
		_, resp, err := g.api.Issues.CreateComment(ctx, g.owner, g.name, n, gogithub.IssueCommentRequest{Body: body})
		return resp, err
	})
}

// Relabel creates missing labels first: GitHub rejects unknown labels on edit.
func (g *githubClient) Relabel(ctx context.Context, n int, add, remove []string) error {
	color, desc := "5319e7", "delamain"
	for _, l := range append(append([]string{}, add...), remove...) {
		err := g.do(ctx, "relabel", func() (*gogithub.Response, error) {
			_, resp, err := g.api.Issues.CreateLabel(ctx, g.owner, g.name, gogithub.CreateIssueLabelRequest{Name: l, Color: &color, Description: &desc})
			if alreadyExists(err) {
				return resp, nil
			}
			return resp, err
		})
		if err != nil {
			return err
		}
	}
	if len(add) > 0 {
		err := g.do(ctx, "relabel", func() (*gogithub.Response, error) {
			_, resp, err := g.api.Issues.AddLabelsToIssue(ctx, g.owner, g.name, n, add)
			return resp, err
		})
		if err != nil {
			return err
		}
	}
	for _, l := range remove {
		err := g.do(ctx, "relabel", func() (*gogithub.Response, error) {
			resp, err := g.api.Issues.RemoveLabelForIssue(ctx, g.owner, g.name, n, l)
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return resp, nil // the issue does not carry the label, which is the state asked for
			}
			return resp, err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// alreadyExists recognizes GitHub's 422 for creating a label that is already there.
func alreadyExists(err error) bool {
	er, ok := errors.AsType[*gogithub.ErrorResponse](err)
	if !ok {
		return false
	}
	for _, e := range er.Errors {
		if e.Code == "already_exists" {
			return true
		}
	}
	return false
}

func (g *githubClient) whoami(ctx context.Context) (string, error) {
	var u *gogithub.User
	err := g.do(ctx, "whoami", func() (resp *gogithub.Response, err error) {
		u, resp, err = g.api.Users.Get(ctx, "")
		return resp, err
	})
	return u.GetLogin(), err
}

// PRView reads one PR. A head without repository is a fork whose repository was deleted.
func (g *githubClient) PRView(ctx context.Context, n int) (PRDetail, error) {
	var v *gogithub.PullRequest
	err := g.do(ctx, "pr view", func() (resp *gogithub.Response, err error) {
		v, resp, err = g.api.PullRequests.Get(ctx, g.owner, g.name, n)
		return resp, err
	})
	if err != nil {
		return PRDetail{}, err
	}
	head, base := v.GetHead(), v.GetBase()
	return PRDetail{
		Number: v.GetNumber(), URL: v.GetHTMLURL(), Title: v.GetTitle(), Body: v.GetBody(), Author: v.GetUser().GetLogin(),
		Draft: v.GetDraft(), Open: v.GetState() == "open",
		Fork:    head.GetRepo() == nil || head.GetRepo().GetFullName() != base.GetRepo().GetFullName(),
		BaseRef: base.GetRef(), HeadRef: head.GetRef(), BaseSHA: base.GetSHA(), HeadSHA: head.GetSHA(), StartSHA: base.GetSHA(),
	}, nil
}

// ListPRs filters by label on the issues endpoint, since the pulls one cannot, and reads each
// PR it finds. Only the first 100 labeled issues are seen.
func (g *githubClient) ListPRs(ctx context.Context, label string) ([]PRDetail, error) {
	var vs []*gogithub.Issue
	err := g.do(ctx, "list prs", func() (resp *gogithub.Response, err error) {
		vs, resp, err = g.api.Issues.ListByRepo(ctx, g.owner, g.name, &gogithub.IssueListByRepoOptions{
			State: "open", Labels: []string{label}, ListOptions: gogithub.ListOptions{PerPage: 100},
		})
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	prs := make([]PRDetail, 0, len(vs))
	for _, v := range vs {
		if !v.IsPullRequest() {
			continue
		}
		pr, err := g.PRView(ctx, v.GetNumber())
		if err != nil {
			return nil, err
		}
		prs = append(prs, pr)
	}
	return oldestPRsFirst(prs), nil
}

const closingIssuesQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){closingIssuesReferences(first:5){nodes{number repository{nameWithOwner}}}}}}`

// LinkedIssues asks GraphQL, the only API that knows which issues a PR closes, and falls back
// to the closing keywords of the body when it knows none.
func (g *githubClient) LinkedIssues(ctx context.Context, n int) ([]Issue, error) {
	var out struct {
		Repository struct {
			PullRequest struct {
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int
						Repository struct{ NameWithOwner string }
					}
				}
			}
		}
	}
	if err := g.graphql(ctx, closingIssuesQuery, map[string]any{"owner": g.owner, "name": g.name, "number": n}, &out); err != nil {
		return nil, err
	}
	var ns []int
	for _, v := range out.Repository.PullRequest.ClosingIssuesReferences.Nodes {
		// An issue of another repository would be read here under the same number.
		if strings.EqualFold(v.Repository.NameWithOwner, g.owner+"/"+g.name) {
			ns = append(ns, v.Number)
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

// issueCommentThread prefixes the thread id of a PR-level comment.
const issueCommentThread = "issue-comment:"

const reviewThreadsQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100){nodes{id isResolved path line comments(first:100){nodes{databaseId author{login} body createdAt}}}}}}}`

// Threads joins the inline review threads, which only GraphQL has, with the PR-level comments
// of the issues endpoint. Only the first 100 of each are read; line is null on outdated threads
// and author on deleted accounts.
func (g *githubClient) Threads(ctx context.Context, n int) ([]Thread, error) {
	var out struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string
						IsResolved bool
						Path       string
						Line       *int
						Comments   struct {
							Nodes []struct {
								DatabaseID int64
								Author     *struct{ Login string }
								Body       string
								CreatedAt  time.Time
							}
						}
					}
				}
			}
		}
	}
	if err := g.graphql(ctx, reviewThreadsQuery, map[string]any{"owner": g.owner, "name": g.name, "number": n}, &out); err != nil {
		return nil, err
	}
	var cs []*gogithub.IssueComment
	err := g.do(ctx, "threads", func() (resp *gogithub.Response, err error) {
		cs, resp, err = g.api.Issues.ListComments(ctx, g.owner, g.name, n, &gogithub.IssueListCommentsOptions{ListOptions: gogithub.ListOptions{PerPage: 100}})
		return resp, err
	})
	if err != nil {
		return nil, err
	}
	nodes := out.Repository.PullRequest.ReviewThreads.Nodes
	ts := make([]Thread, 0, len(nodes)+len(cs))
	for _, v := range nodes {
		t := Thread{ID: v.ID, Path: v.Path, Resolved: v.IsResolved}
		if v.Line != nil {
			t.Line = *v.Line
		}
		for _, c := range v.Comments.Nodes {
			cm := Comment{ID: strconv.FormatInt(c.DatabaseID, 10), Body: c.Body, CreatedAt: c.CreatedAt}
			if c.Author != nil {
				cm.Author = c.Author.Login
			}
			t.Comments = append(t.Comments, cm)
		}
		ts = append(ts, t)
	}
	for _, c := range cs {
		id := strconv.FormatInt(c.GetID(), 10)
		ts = append(ts, Thread{ID: issueCommentThread + id, Comments: []Comment{
			{ID: id, Author: c.GetUser().GetLogin(), Body: c.GetBody(), CreatedAt: c.GetCreatedAt().Time, Bot: c.GetUser().GetType() == "Bot"},
		}})
	}
	return byFirstComment(ts), nil
}

// reviewEvent is the only review event delamain sends: it never approves nor requests changes.
const reviewEvent = "COMMENT"

// PostReview sends the summary and every inline comment in a single review.
func (g *githubClient) PostReview(ctx context.Context, n int, o ReviewOptions) error {
	event, side := reviewEvent, "RIGHT"
	req := &gogithub.PullRequestReviewRequest{CommitID: &o.HeadSHA, Body: &o.Summary, Event: &event}
	bodies := []string{o.Summary}
	for _, c := range o.Comments {
		req.Comments = append(req.Comments, &gogithub.DraftReviewComment{Path: &c.Path, Line: &c.Line, Side: &side, Body: &c.Body})
		bodies = append(bodies, c.Body)
	}
	return g.do(ctx, "post review", func() (*gogithub.Response, error) {
		_, resp, err := g.api.PullRequests.CreateReview(ctx, g.owner, g.name, n, req)
		return resp, err
	}, bodies...)
}

// PRComment uses the issues endpoint: on GitHub's API a PR is an issue.
func (g *githubClient) PRComment(ctx context.Context, n int, body string) error {
	return g.do(ctx, "pr comment", func() (*gogithub.Response, error) {
		_, resp, err := g.api.Issues.CreateComment(ctx, g.owner, g.name, n, gogithub.IssueCommentRequest{Body: body})
		return resp, err
	}, body)
}

const firstThreadCommentQuery = `query($id:ID!){node(id:$id){... on PullRequestReviewThread{comments(first:1){nodes{databaseId}}}}}`

// Reply answers a review thread by replying to its first comment, whose REST id only GraphQL
// knows. A PR-level comment has no thread on GitHub: the answer is a new PR comment.
func (g *githubClient) Reply(ctx context.Context, n int, threadID, body string) error {
	if strings.HasPrefix(threadID, issueCommentThread) {
		return g.PRComment(ctx, n, body)
	}
	var out struct {
		Node *struct {
			Comments struct{ Nodes []struct{ DatabaseID int64 } }
		}
	}
	if err := g.graphql(ctx, firstThreadCommentQuery, map[string]any{"id": threadID}, &out); err != nil {
		return err
	}
	if out.Node == nil || len(out.Node.Comments.Nodes) == 0 {
		return fmt.Errorf("github reply: thread %q has no comment to reply to", threadID)
	}
	return g.do(ctx, "reply", func() (*gogithub.Response, error) {
		_, resp, err := g.api.PullRequests.CreateCommentInReplyTo(ctx, g.owner, g.name, n, body, out.Node.Comments.Nodes[0].DatabaseID)
		return resp, err
	}, body)
}

const resolveThreadMutation = `mutation($threadId:ID!){resolveReviewThread(input:{threadId:$threadId}){thread{isResolved}}}`

// ResolveThread resolves a review thread; a PR-level comment has nothing to resolve.
func (g *githubClient) ResolveThread(ctx context.Context, _ int, threadID string) error {
	if strings.HasPrefix(threadID, issueCommentThread) {
		return nil
	}
	var out struct{}
	return g.graphql(ctx, resolveThreadMutation, map[string]any{"threadId": threadID}, &out)
}

// RelabelPR is Relabel: on GitHub's API a PR is an issue.
func (g *githubClient) RelabelPR(ctx context.Context, n int, add, remove []string) error {
	return g.Relabel(ctx, n, add, remove)
}
