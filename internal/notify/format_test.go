package notify

import (
	"reflect"
	"testing"
)

const (
	examplePR    = "https://github.com/paraizofelipe/delamain/pull/31"
	exampleIssue = "https://github.com/paraizofelipe/delamain/issues/12"
)

// example is the run of the validated examples in the issue.
func example() Run {
	return Run{
		ID: 42, Repo: "/home/dev/projects/delamain", Ref: "#12", Title: "Mostrar HARNESS e INTERACTIONS no status",
		IssueURL: exampleIssue, PRURL: examplePR, Iterations: 2, MaxIterations: 3, Tokens: "167K", CostUSD: 0.67,
	}
}

func assertMessage(t *testing.T, got, want Message) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("toast mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFormatPROpenedShouldMatchContract(t *testing.T) {
	assertMessage(t, FormatPROpened(example()), Message{
		Event: EventPROpened, RunID: 42,
		Title:   "✅ PR opened · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nPR #31 · 2 iterations · 167K tokens · US$ 0.67",
		Urgency: Normal, Expire: 15000, Category: "x-delamain.pr",
		Actions: []Action{
			{Key: "default", Label: "Open PR", Target: examplePR},
			{Key: "open-pr", Label: "Open PR", Target: examplePR},
			{Key: "open-issue", Label: "Open issue", Target: exampleIssue},
		},
	})
}
