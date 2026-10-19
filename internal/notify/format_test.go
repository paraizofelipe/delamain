package notify

import (
	"reflect"
	"strings"
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

func TestFormatShouldTruncateTitleBodyAndReasonWithEllipsis(t *testing.T) {
	r := example()
	r.PRURL, r.State, r.Iterations, r.Report = "", "plan", 1, exampleReport
	r.Ref = "task:" + strings.Repeat("a", 100) // the toast title would have 125 runes
	r.Title = strings.Repeat("é", 70)          // multibyte: runes are counted, not bytes
	r.Reason = strings.Repeat("x", 130)

	m := FormatHalted(r)
	if want := "⛔ Halted · delamain task:" + strings.Repeat("a", 54) + "…"; m.Title != want {
		t.Fatalf("title cut at 80 runes:\n got %q\nwant %q", m.Title, want)
	}
	want := strings.Repeat("é", 59) + "…\nrun 42 · plan · 1/3 iterations\n" + strings.Repeat("x", 119) + "…"
	if m.Body != want {
		t.Fatalf("line 1 cut at 60 and the reason at 120 runes:\n got %q\nwant %q", m.Body, want)
	}

	r = example()
	r.Title, r.Reason, r.State, r.PRURL = strings.Repeat("b", 60), "", "plan", ""
	if m := FormatHalted(r); m.Body != strings.Repeat("b", 60)+"\nrun 42 · plan · 2/3 iterations" {
		t.Fatalf("a title of exactly 60 runes stays whole and an empty reason drops line 3: %q", m.Body)
	}
}

func TestFormatNeedsHumanWithoutPRShouldOfferReportAndIssue(t *testing.T) {
	r := example()
	r.PRURL, r.Report = "", exampleReport
	assertMessage(t, FormatNeedsHuman(r), Message{
		Event: EventNeedsHuman, RunID: 42,
		Title:   "⚠️ Needs review · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nNo PR · 2/3 iterations · 167K tokens",
		Urgency: Normal, Expire: 15000, Category: "x-delamain.needs-human",
		Actions: []Action{
			{Key: "default", Label: "Open report", Target: exampleReport},
			{Key: "open-report", Label: "Open report", Target: exampleReport},
			{Key: "open-issue", Label: "Open issue", Target: exampleIssue},
		},
	})
}

func TestFormatTaskWithoutIssueShouldNotOfferOpenIssue(t *testing.T) {
	r := example()
	r.Ref, r.IssueURL, r.Report = "task:notes", "", exampleReport
	report := []Action{{Key: "default", Label: "Open report", Target: exampleReport}, {Key: "open-report", Label: "Open report", Target: exampleReport}}
	pr := []Action{{Key: "default", Label: "Open PR", Target: examplePR}, {Key: "open-pr", Label: "Open PR", Target: examplePR}}

	if got := FormatPROpened(r).Actions; !reflect.DeepEqual(got, pr) {
		t.Fatalf("pr_opened actions = %+v", got)
	}
	if got := FormatHalted(r).Actions; !reflect.DeepEqual(got, report) {
		t.Fatalf("halted actions = %+v", got)
	}
	r.PRURL = ""
	if got := FormatNeedsHuman(r).Actions; !reflect.DeepEqual(got, report) {
		t.Fatalf("without PR nor issue, the default action opens the report: %+v", got)
	}
	if got := FormatStarted(r).Actions; got != nil {
		t.Fatalf("started without issue has nothing to open: %+v", got)
	}
	if m := FormatPROpened(r); m.Title != "✅ PR opened · delamain task:notes" {
		t.Fatalf("title = %q", m.Title)
	}
}

func TestFormatShouldFallBackToPRWhenTheURLHasNoNumber(t *testing.T) {
	cases := map[string]string{
		"https://gitlab.com/grp/api/-/merge_requests/7": "PR #7 · ",
		"https://github.com/acme/api/pull/42/":          "PR #42 · ",
		"https://github.com/acme/api/pull/new":          "PR · ",
	}
	for url, want := range cases {
		r := example()
		r.PRURL = url
		if _, line2, _ := strings.Cut(FormatPROpened(r).Body, "\n"); !strings.HasPrefix(line2, want) {
			t.Errorf("%s: line 2 = %q, want prefix %q", url, line2, want)
		}
	}
	r := example()
	r.PRURL, r.Draft = "https://github.com/acme/api/pull/new", true
	if _, line2, _ := strings.Cut(FormatNeedsHuman(r).Body, "\n"); !strings.HasPrefix(line2, "Draft PR · ") {
		t.Errorf("draft line 2 = %q", line2)
	}
}
