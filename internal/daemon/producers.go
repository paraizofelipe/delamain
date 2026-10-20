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
	return []Producer{issues.jobs}
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
