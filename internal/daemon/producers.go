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

// ReviewState is what the store knows about the reviews of one PR/MR.
type ReviewState struct {
	ReviewedHead string // head of the last review that posted ("" before the first one)
	HaltedHead   string // head of the last review that halted without posting
	Halts        int    // how many times HaltedHead halted
}

// maxHaltsPerHead is how many halted reviews of the same head the daemon starts before leaving
// that head alone. A halt is usually deterministic (a head that moved, a rejected artifact), so
// retrying it on every poll would spend the daily budget the issues share without ever posting.
// `delamain review --pr n` still reviews it on demand, and a new push starts the count over.
const maxHaltsPerHead = 3

// ReviewJobs reviews the PRs/MRs labeled Label, once per head. State is what earlier reviews of
// a PR recorded. RefuseForks leaves out the PRs/MRs whose head is in a fork, which the runner
// reviews only on a harness that confines the reviewer.
type ReviewJobs struct {
	Enabled     bool
	Label       string
	RefuseForks bool
	State       func(ctx context.Context, repo string, pr int) (ReviewState, error)
	Run         func(ctx context.Context, repo string, pr int, fg forge.Forge) (pipeline.Result, error)
}

// Producers is the daemon's job order: issues first, then reviews when enabled.
func Producers(log *slog.Logger, issues IssueJobs, reviews ReviewJobs) []Producer {
	ps := []Producer{issues.jobs}
	if reviews.Enabled {
		// A poll runs one producer at a time, so this set needs no lock (as budgetAlert.sent).
		warned := map[string]bool{}
		ps = append(ps, func(ctx context.Context, src Source) ([]Job, error) {
			return reviews.jobs(ctx, log, warned, src)
		})
	}
	return ps
}

// jobs lists the source's ready issues, oldest first.
func (o IssueJobs) jobs(ctx context.Context, src Source) ([]Job, error) {
	is, err := src.Forge.ListReady(ctx, o.Label)
	if err != nil {
		return nil, fmt.Errorf("list the issues labeled %s: %w", o.Label, err)
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

// jobs lists one review per PR/MR of the source carrying the review label, leaving drafts out,
// the forks RefuseForks rules out, the heads already reviewed and the heads whose review keeps
// halting. When a PR's state cannot be read, no review of the source starts: reviewing the same
// head twice costs budget and reposts comments. warned keeps the fork notice to once per PR.
func (o ReviewJobs) jobs(ctx context.Context, log *slog.Logger, warned map[string]bool, src Source) ([]Job, error) {
	prs, err := src.Forge.ListPRs(ctx, o.Label)
	if err != nil {
		return nil, fmt.Errorf("list the PRs/MRs labeled %s: %w", o.Label, err)
	}
	out := make([]Job, 0, len(prs))
	for _, pr := range prs {
		if pr.Draft {
			continue
		}
		// The runner refuses a fork on this harness before it records anything, so a job for
		// it would fail on every poll without ever reviewing the PR.
		if pr.Fork && o.RefuseForks {
			if key := fmt.Sprintf("%s#%d", src.Repo, pr.Number); !warned[key] {
				warned[key] = true
				log.Warn("PR/MR from a fork needs a harness that confines the reviewer",
					"repo", src.Repo, "pr", pr.Number)
			}
			continue
		}
		st, err := o.State(ctx, src.Repo, pr.Number)
		if err != nil {
			return nil, fmt.Errorf("PR/MR #%d: state of the last review: %w", pr.Number, err)
		}
		if pr.HeadSHA == "" {
			// Nothing to compare: review it and let RunReview decide.
			out = append(out, o.job(src, pr.Number))
			continue
		}
		if pr.HeadSHA == st.ReviewedHead {
			log.Debug("PR/MR head already reviewed", "repo", src.Repo, "pr", pr.Number, "head", pr.HeadSHA)
			continue
		}
		if pr.HeadSHA == st.HaltedHead && st.Halts >= maxHaltsPerHead {
			log.Warn("PR/MR head left alone after halted reviews", "repo", src.Repo, "pr", pr.Number,
				"head", pr.HeadSHA, "halts", st.Halts)
			continue
		}
		out = append(out, o.job(src, pr.Number))
	}
	return out, nil
}

// job is the review of PR/MR n of src.
func (o ReviewJobs) job(src Source, n int) Job {
	return Job{
		Key: fmt.Sprintf("%s#pr:%d", src.Repo, n),
		Run: func(ctx context.Context) (pipeline.Result, error) { return o.Run(ctx, src.Repo, n, src.Forge) },
	}
}
