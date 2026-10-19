package notify

import (
	"reflect"
	"testing"
)

const (
	samplePR    = "https://github.com/paraizofelipe/delamain/pull/31"
	sampleIssue = "https://github.com/paraizofelipe/delamain/issues/12"
)

// sampleRun is the run of the issue's validated examples.
func sampleRun() Run {
	return Run{
		ID: 42, Repo: "/home/dev/projects/delamain", Ref: "#12", Title: "Mostrar HARNESS e INTERACTIONS no status",
		Harness: "omp", State: "implement", Iterations: 2, MaxIterations: 3, Tokens: "167K", CostUSD: 0.67,
		PRURL: samplePR, IssueURL: sampleIssue, ReportPath: "/state/runs/delamain/42/report.md",
	}
}

func assertMessage(t *testing.T, got, want Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("message mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFormatPROpenedShouldMatchContract(t *testing.T) {
	assertMessage(t, FormatPROpened(sampleRun()), Message{
		Event: EventPROpened, RunID: 42,
		Title:   "✅ PR opened · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nPR #31 · 2 iterations · 167K tokens · US$ 0.67",
		Urgency: Normal, Expire: 15000, Category: "x-delamain.pr",
		Actions: []Action{
			{Key: "default", Label: "Open PR", Target: samplePR},
			{Key: "open-pr", Label: "Open PR", Target: samplePR},
			{Key: "open-issue", Label: "Open issue", Target: sampleIssue},
		},
	})
}
