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
)

func newReviewCmd(load func() (config.Config, error)) *cobra.Command {
	var repo, output string
	var pr int
	cmd := &cobra.Command{
		Use:   "review --pr <n> [--repo <path>]",
		Short: "Review a PR/MR read-only and post comments (never approves nor merges)",
		Long: `Reviews any open PR/MR of the origin forge, forks included. The PR head is fetched into
refs/delamain/pr/<n> and checked out in a detached worktree in paths.worktrees_dir; the
pr-reviewer agent reads it (spec, standards and security, then a verifier) without running any of
the PR's code. delamain then posts one comment-only review: inline comments for critical/major
findings on changed lines, everything else in the summary. Requires review.enabled.

Exit codes: 0 reviewed or skipped · 1 interrupted.`,
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
			res, err := reviewPR(c.Context(), cfg, repo, pr, fg)
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

func reviewPR(ctx context.Context, cfg config.Config, repo string, pr int, fg forge.Forge) (pipeline.Result, error) {
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
	r.Forge = fg
	return r.RunReview(ctx, repo, pr)
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
		icon := map[pipeline.Outcome]string{pipeline.OutcomeReviewed: "✅", pipeline.OutcomeSkipped: "⏭️ ", pipeline.OutcomeHalted: "❌"}[res.Outcome]
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
