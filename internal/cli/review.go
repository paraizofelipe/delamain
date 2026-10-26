package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

func newReviewCmd(load func() (config.Config, error)) *cobra.Command {
	var repo, output string
	var pr int
	cmd := &cobra.Command{
		Use:   "review --pr <n> [--repo <path>]",
		Short: "Review a PR/MR, or answer the review comments of one delamain opened",
		Long: `What this does depends on who opened the PR/MR, and nothing else triggers it.

A PR/MR delamain did not open is reviewed, as long as its head is a branch of this repository:
one whose head lives in a fork is refused with an error, on every harness, because its code
answers to nobody here. The head is fetched into refs/delamain/pr/<n> and checked out in a
detached worktree in paths.worktrees_dir; the pr-reviewer agent reads it (spec, standards and
security, then a verifier) without running any of the PR's code. delamain then posts one
comment-only review, never an approval nor a merge: every finding on a line the head touched
becomes an inline comment, and a finding the forge cannot anchor goes to the summary with the
excerpt of the code it is about.

A PR/MR delamain opened has its pending review threads answered instead — the ones whose last
comment is not delamain's own. Each gets one reply with a verdict: valid (the comment holds),
invalid (it does not, with the evidence read in the code) or unclear (what is missing to decide).
With review.fix on, the valid ones are also fixed: one commit per thread, pushed as a
fast-forward once lint, typecheck, tests and the secret scan pass, and those threads are
resolved. Answering requires agent.harness: claude, the only harness that confines the
responder, since whoever comments on the PR writes what it reads.

Requires review.enabled.

Exit codes: 0 reviewed, answered or skipped · 1 interrupted.`,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return err
			}
			if pr <= 0 {
				return errors.New("provide the PR/MR number with --pr <n>")
			}
			if !cfg.Review.Enabled {
				return errors.New("review is disabled: set review.enabled: true in the config")
			}
			if !slices.Contains(outputFormats, output) {
				return fmt.Errorf("invalid format %q: use %s", output, strings.Join(outputFormats, "|"))
			}
			if repo == "" {
				if repo, err = os.Getwd(); err != nil {
					return err
				}
			}
			fg, err := forgeFor(c.Context(), cfg, repo)
			if err != nil {
				return err
			}
			if fg == nil {
				return errors.New("review requires a GitHub or GitLab origin")
			}
			// Only comments by this login count as delamain's own when deduplicating.
			id, err := forgeAuth(c.Context(), cfg, repo)
			if err != nil {
				return fmt.Errorf("could not identify the forge user that posts the review: %w", err)
			}
			res, err := reviewPR(c.Context(), cfg, repo, pr, fg, id.Login)
			if err != nil {
				return err
			}
			if err := printReviewResult(c.OutOrStdout(), output, res); err != nil {
				return err
			}
			if code := outcomeExit[res.Outcome]; code != 0 {
				return ExitError{Code: code}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&pr, "pr", 0, "PR/MR number in the origin forge")
	cmd.Flags().StringVar(&repo, "repo", "", "target repository (default: current directory)")
	addOutputFlag(cmd, &output)
	return cmd
}

func reviewPR(ctx context.Context, cfg config.Config, repo string, pr int, fg forge.Forge, user string) (pipeline.Result, error) {
	cfg, _, err := projectConfig(cfg, repo)
	if err != nil {
		return pipeline.Result{}, err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = st.Close() }()
	r, err := newRunner(cfg, st, notify.Options{})
	if err != nil {
		return pipeline.Result{}, err
	}
	defer func() { _ = r.Notify.Close() }()
	r.Forge, r.ForgeUser = fg, user
	own, err := openedByDelamain(ctx, st, fg, pr)
	if err != nil {
		return pipeline.Result{}, err
	}
	if own {
		// Reviewing its own code again would only repeat the review that opened the PR; what is
		// left to do there is to answer whoever reviewed it.
		return r.RunAddress(ctx, repo, pr)
	}
	return r.RunReview(ctx, repo, pr)
}

// openedByDelamain reports whether PR/MR pr is one an issue run of delamain opened, which is
// what decides between answering its threads and reviewing it.
//
// The PR's own URL identifies it, as it does in fixTarget. Not the PR's author: delamain posts
// with the operator's own forge credential, so every PR the operator opened by hand carries the
// same author. And not the repository path either: the same repository is checked out at
// several paths — every worktree is one — while a run records the path it ran from, so matching
// on it answered "not delamain's" for a PR of its own and posted a review on it.
func openedByDelamain(ctx context.Context, st *store.Store, fg forge.Forge, pr int) (bool, error) {
	// One extra read of the PR, before the stage reads it again: cheap next to a review that
	// should not have been posted.
	d, err := fg.PRView(ctx, pr)
	if err != nil {
		return false, err
	}
	return st.OpenedMR(ctx, d.URL)
}

// ReviewView is the outcome of `delamain review` as json/yaml show it.
type ReviewView struct {
	RunID     int64    `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Outcome   string   `json:"outcome" yaml:"outcome"`
	Reason    string   `json:"reason,omitempty" yaml:"reason,omitempty"`
	PR        string   `json:"pr,omitempty" yaml:"pr,omitempty"`
	Inline    int      `json:"inline" yaml:"inline"`
	InSummary int      `json:"in_summary" yaml:"in_summary"`
	Dropped   int      `json:"dropped" yaml:"dropped"`
	Usage     string   `json:"usage,omitempty" yaml:"usage,omitempty"`
	Warnings  []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
	Report    string   `json:"report,omitempty" yaml:"report,omitempty"`
}

func reviewView(res pipeline.Result) ReviewView {
	v := ReviewView{RunID: res.RunID, Outcome: string(res.Outcome), Reason: res.Reason, PR: res.PRURL, Usage: res.Budget, Warnings: res.Warnings, Report: res.ReportPath}
	if s := res.Review; s != nil {
		v.PR, v.Inline, v.InSummary, v.Dropped = s.PRURL, s.Inline, s.InSummary, s.Dropped
	}
	return v
}

func printReviewResult(w io.Writer, format string, res pipeline.Result) error {
	v := reviewView(res)
	return printAs(w, format, v, func(w io.Writer) error {
		icon := map[pipeline.Outcome]string{pipeline.OutcomeReviewed: "✅", pipeline.OutcomeAnswered: "💬", pipeline.OutcomeSkipped: "⏭️ ", pipeline.OutcomeHalted: "❌"}[res.Outcome]
		fmt.Fprintf(w, "%s %s", icon, v.Outcome)
		if v.Reason != "" {
			fmt.Fprintf(w, " — %s", v.Reason)
		}
		fmt.Fprintln(w)
		if v.PR != "" {
			fmt.Fprintf(w, "   PR/MR:      %s\n", v.PR)
		}
		if res.Review != nil {
			fmt.Fprintf(w, "   comments:   inline: %d · summary: %d · dropped: %d\n", v.Inline, v.InSummary, v.Dropped)
		}
		if v.Usage != "" {
			fmt.Fprintf(w, "   usage:      %s\n", v.Usage)
		}
		for _, warn := range v.Warnings {
			fmt.Fprintf(w, "   ⚠️  %s\n", warn)
		}
		if v.Report != "" {
			fmt.Fprintf(w, "   report:     %s\n", v.Report)
		}
		return nil
	})
}
