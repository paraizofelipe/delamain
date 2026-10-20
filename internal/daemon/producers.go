package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
)

// Job is one unit of work the daemon schedules. Key identifies it while it is in flight, so the
// next poll does not start it again.
type Job struct {
	Key string
	Run func(ctx context.Context) (pipeline.Result, error)
}

// Producer lists the jobs a source has ready now.
type Producer func(ctx context.Context, src Source) ([]Job, error)

// IssueJobs runs the issues labeled Label.
type IssueJobs struct {
	Label string
	Run   func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error)
}

// ReviewJobs reviews the PRs/MRs labeled Label, once per head. LastHead is the head the last
// review of a PR saw ("" before the first one).
type ReviewJobs struct {
	Enabled  bool
	Label    string
	LastHead func(ctx context.Context, repo string, pr int) (string, error)
	Run      func(ctx context.Context, repo string, pr int, fg forge.Forge) (pipeline.Result, error)
}

// Producers is the daemon's job order: issues first, then reviews when enabled.
func Producers(log *slog.Logger, issues IssueJobs, reviews ReviewJobs) []Producer {
	ps := []Producer{issues.jobs}
	if reviews.Enabled {
		ps = append(ps, func(ctx context.Context, src Source) ([]Job, error) { return reviews.jobs(ctx, log, src) })
	}
	return ps
}

// jobs lists the source's ready issues, oldest first.
func (o IssueJobs) jobs(ctx context.Context, src Source) ([]Job, error) {
	is, err := src.Forge.ListReady(ctx, o.Label)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(is))
	for _, i := range is {
		out = append(out, Job{
			Key: fmt.Sprintf("%s#%d", src.Repo, i.Number),
			Run: func(ctx context.Context) (pipeline.Result, error) {
				return o.Run(ctx, pipeline.TaskFromIssue(i, src.Repo), src.Forge)
			},
		})
	}
	return out, nil
}

// jobs lists one review per PR/MR of the source carrying the review label, leaving drafts out and
// those whose head was already reviewed. When a PR's last head cannot be read, no review of the
// source starts: reviewing the same head twice costs budget and reposts comments.
func (o ReviewJobs) jobs(ctx context.Context, log *slog.Logger, src Source) ([]Job, error) {
	prs, err := src.Forge.ListPRs(ctx, o.Label)
	if err != nil {
		return nil, err
	}
	out := make([]Job, 0, len(prs))
	for _, pr := range prs {
		if pr.Draft {
			continue
		}
		last, err := o.LastHead(ctx, src.Repo, pr.Number)
		if err != nil {
			return nil, fmt.Errorf("PR/MR #%d: last reviewed head: %w", pr.Number, err)
		}
		if pr.HeadSHA != "" && pr.HeadSHA == last {
			log.Debug("PR/MR head already reviewed", "repo", src.Repo, "pr", pr.Number, "head", last)
			continue
		}
		out = append(out, Job{
			Key: fmt.Sprintf("%s#pr:%d", src.Repo, pr.Number),
			Run: func(ctx context.Context) (pipeline.Result, error) { return o.Run(ctx, src.Repo, pr.Number, src.Forge) },
		})
	}
	return out, nil
}
