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

const exampleReport = "/home/dev/.local/state/delamain/runs/delamain/42/report.md"

func TestFormatNeedsHumanShouldMatchContract(t *testing.T) {
	r := example()
	r.Draft, r.Iterations, r.Tokens, r.Reason, r.Report = true, 3, "1.2M", "3 iterações sem aprovação", exampleReport
	assertMessage(t, FormatNeedsHuman(r), Message{
		Event: EventNeedsHuman, RunID: 42,
		Title:   "⚠️ Needs review · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nDraft PR #31 · 3/3 iterations · 1.2M tokens\n3 iterações sem aprovação",
		Urgency: Normal, Expire: 15000, Category: "x-delamain.needs-human",
		Actions: []Action{
			{Key: "default", Label: "Open PR", Target: examplePR},
			{Key: "open-pr", Label: "Open PR", Target: examplePR},
			{Key: "open-report", Label: "Open report", Target: exampleReport},
		},
	})
}

func TestFormatHaltedShouldBeCriticalAndNeverExpire(t *testing.T) {
	r := example()
	r.PRURL, r.State, r.Reason, r.Report = "", "implement", "reviewer alterou a worktree", exampleReport
	assertMessage(t, FormatHalted(r), Message{
		Event: EventHalted, RunID: 42,
		Title:   "⛔ Halted · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nrun 42 · implement · 2/3 iterations\nreviewer alterou a worktree",
		Urgency: Critical, Expire: 0, Category: "x-delamain.halted",
		Actions: []Action{
			{Key: "default", Label: "Open report", Target: exampleReport},
			{Key: "open-report", Label: "Open report", Target: exampleReport},
			{Key: "open-issue", Label: "Open issue", Target: exampleIssue},
		},
	})
}

func TestFormatNeedsInfoShouldMatchContract(t *testing.T) {
	r := example()
	r.ID, r.PRURL, r.Label = 0, "", "delamain:needs-info"
	assertMessage(t, FormatNeedsInfo(r), Message{
		Event:   EventNeedsInfo,
		Title:   "ℹ️ Needs info · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nIssue relabeled delamain:needs-info",
		Urgency: Low, Expire: 15000, Category: "x-delamain.needs-info",
		Actions: []Action{
			{Key: "default", Label: "Open issue", Target: exampleIssue},
			{Key: "open-issue", Label: "Open issue", Target: exampleIssue},
		},
	})
}

func TestFormatStartedShouldBeLowUrgencyWithRunAndHarness(t *testing.T) {
	r := example()
	r.PRURL, r.Harness = "", "claude"
	assertMessage(t, FormatStarted(r), Message{
		Event: EventStarted, RunID: 42,
		Title:   "▶️ Started · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nrun 42 · claude",
		Urgency: Low, Expire: 15000, Category: "x-delamain.started",
		Actions: []Action{{Key: "default", Label: "Open issue", Target: exampleIssue}},
	})
}
