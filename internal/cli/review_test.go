package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/pipeline"
)

func reviewConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReviewShouldRequirePR(t *testing.T) {
	cfg := reviewConfig(t, "review:\n  enabled: true\n")
	for _, args := range [][]string{{"review"}, {"review", "--pr", "0"}, {"review", "--pr", "-3"}} {
		_, err := execute(t, append([]string{"--config", cfg}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "--pr") {
			t.Errorf("%v: a review needs a PR number, got %v", args, err)
		}
	}
}

func TestReviewShouldFailWhenReviewIsDisabled(t *testing.T) {
	cfg := reviewConfig(t, "review:\n  enabled: false\n")
	_, err := execute(t, "--config", cfg, "review", "--pr", "5", "--repo", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "review.enabled") {
		t.Fatalf("a disabled review must say how to enable it, got %v", err)
	}
}

func TestReviewShouldFailWithoutForge(t *testing.T) {
	cfg := reviewConfig(t, "review:\n  enabled: true\nagent:\n  harness: claude\nforge:\n  kind: none\n")
	_, err := execute(t, "--config", cfg, "review", "--pr", "5", "--repo", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "review requires a GitHub or GitLab origin") {
		t.Fatalf("got %v", err)
	}
	if _, err := execute(t, "--config", cfg, "review", "--pr", "5", "-o", "xml"); err == nil || !strings.Contains(err.Error(), "xml") {
		t.Fatalf("an unknown output format is refused before any work: %v", err)
	}
}

func TestPrintReviewResultShouldShowCountsLinkAndReportInEveryFormat(t *testing.T) {
	res := pipeline.Result{
		RunID: 12, Outcome: pipeline.OutcomeReviewed, ReportPath: "/state/runs/api/12/report.md", Budget: "90K de 500K tokens",
		Review: &pipeline.ReviewSummary{PRURL: "https://github.com/acme/api/pull/5", Inline: 2, InSummary: 3, Dropped: 1},
	}
	var table bytes.Buffer
	if err := printReviewResult(&table, "table", res); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reviewed", "https://github.com/acme/api/pull/5", "inline: 2", "summary: 3", "dropped: 1", "/state/runs/api/12/report.md"} {
		if !strings.Contains(table.String(), want) {
			t.Errorf("table must show %q:\n%s", want, table.String())
		}
	}

	type view struct {
		RunID     int64  `json:"run_id" yaml:"run_id"`
		Outcome   string `json:"outcome" yaml:"outcome"`
		PR        string `json:"pr" yaml:"pr"`
		Inline    int    `json:"inline" yaml:"inline"`
		InSummary int    `json:"in_summary" yaml:"in_summary"`
		Dropped   int    `json:"dropped" yaml:"dropped"`
		Report    string `json:"report" yaml:"report"`
	}
	want := view{RunID: 12, Outcome: "reviewed", PR: "https://github.com/acme/api/pull/5", Inline: 2, InSummary: 3, Dropped: 1, Report: "/state/runs/api/12/report.md"}
	for format, decode := range map[string]func([]byte, any) error{"json": json.Unmarshal, "yaml": yaml.Unmarshal} {
		var out bytes.Buffer
		if err := printReviewResult(&out, format, res); err != nil {
			t.Fatal(err)
		}
		var got view
		if err := decode(out.Bytes(), &got); err != nil || got != want {
			t.Errorf("%s: got %+v (%v), want %+v\n%s", format, got, err, want, out.String())
		}
	}

	var skipped bytes.Buffer
	if err := printReviewResult(&skipped, "table", pipeline.Result{Outcome: pipeline.OutcomeSkipped, Reason: "o head já foi revisado"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(skipped.String(), "skipped") || !strings.Contains(skipped.String(), "o head já foi revisado") {
		t.Fatalf("a skip shows why:\n%s", skipped.String())
	}
}

// Only a head in a fork brings third-party code, and RunReview is what refuses that one. The
// command reviews this repository's own PRs/MRs on either harness, so it must not stop at omp.
func TestReviewShouldNotRefuseAnUnconfinedHarnessBeforeSeeingThePR(t *testing.T) {
	cfg := reviewConfig(t, "review:\n  enabled: true\nagent:\n  harness: omp\nforge:\n  kind: none\n")
	_, err := execute(t, "--config", cfg, "review", "--pr", "5", "--repo", t.TempDir())
	if err == nil || strings.Contains(err.Error(), "agent.harness") {
		t.Fatalf("omp must get past the harness check: %v", err)
	}
	if !strings.Contains(err.Error(), "GitHub or GitLab origin") {
		t.Fatalf("the command stops at the missing forge, got %v", err)
	}
}

func TestRouteReviewShouldAnswerOnlyWhenTheHeadsReviewAlreadyPosted(t *testing.T) {
	ctx := t.Context()
	answered := pipeline.Result{Outcome: pipeline.OutcomeAnswered}
	answer := func(calls *int) func(context.Context, string, int) (pipeline.Result, error) {
		return func(context.Context, string, int) (pipeline.Result, error) { *calls++; return answered, nil }
	}
	for name, tc := range map[string]struct {
		review  pipeline.Result
		err     error
		want    pipeline.Outcome
		chained int
	}{
		"new head reviewed": {review: pipeline.Result{Outcome: pipeline.OutcomeReviewed}, want: pipeline.OutcomeReviewed},
		"head already reviewed": {review: pipeline.Result{Outcome: pipeline.OutcomeSkipped, Skip: pipeline.SkipHeadReviewed},
			want: pipeline.OutcomeAnswered, chained: 1},
		"skipped for another reason": {review: pipeline.Result{Outcome: pipeline.OutcomeSkipped, Reason: "o PR/MR #5 não está aberto"},
			want: pipeline.OutcomeSkipped},
		"review failed": {review: pipeline.Result{}, err: errors.New("forge fora do ar"), want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			var chained int
			review := func(context.Context, string, int) (pipeline.Result, error) { return tc.review, tc.err }
			res, err := routeReview(ctx, "/repo", 5, review, answer(&chained))
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("the review's error surfaces untouched: %v", err)
			}
			if res.Outcome != tc.want || chained != tc.chained {
				t.Fatalf("outcome %q (answer called %d×), want %q (%d×)", res.Outcome, chained, tc.want, tc.chained)
			}
		})
	}
}
