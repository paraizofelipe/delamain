package notify

import (
	"reflect"
	"strings"
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

func TestFormatStartedShouldMatchContract(t *testing.T) {
	assertMessage(t, FormatStarted(sampleRun()), Message{
		Event: EventStarted, RunID: 42,
		Title:   "▶️ Started · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nrun 42 · omp",
		Urgency: Low, Expire: 15000, Category: "x-delamain.started",
		Actions: []Action{{Key: "default", Label: "Open issue", Target: sampleIssue}},
	})
}

func TestFormatNeedsHumanShouldMatchContract(t *testing.T) {
	r := sampleRun()
	r.Draft, r.Iterations, r.Tokens, r.CostUSD, r.Reason = true, 3, "1.2M", 4.1, "3 iterações sem aprovação"
	assertMessage(t, FormatNeedsHuman(r), Message{
		Event: EventNeedsHuman, RunID: 42,
		Title:   "⚠️ Needs review · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nDraft PR #31 · 3/3 iterations · 1.2M tokens\n3 iterações sem aprovação",
		Urgency: Normal, Expire: 15000, Category: "x-delamain.needs-human",
		Actions: []Action{
			{Key: "default", Label: "Open PR", Target: samplePR},
			{Key: "open-pr", Label: "Open PR", Target: samplePR},
			{Key: "open-report", Label: "Open report", Target: "/state/runs/delamain/42/report.md"},
		},
	})
}

func TestFormatHaltedShouldBeCriticalAndNeverExpire(t *testing.T) {
	r := sampleRun()
	r.PRURL, r.Reason = "", "reviewer alterou a worktree"
	assertMessage(t, FormatHalted(r), Message{
		Event: EventHalted, RunID: 42,
		Title:   "⛔ Halted · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nrun 42 · implement · 2/3 iterations\nreviewer alterou a worktree",
		Urgency: Critical, Expire: 0, Category: "x-delamain.halted",
		Actions: []Action{
			{Key: "default", Label: "Open report", Target: "/state/runs/delamain/42/report.md"},
			{Key: "open-report", Label: "Open report", Target: "/state/runs/delamain/42/report.md"},
			{Key: "open-issue", Label: "Open issue", Target: sampleIssue},
		},
	})
}

func TestFormatNeedsInfoShouldMatchContract(t *testing.T) {
	r := sampleRun()
	r.ID, r.PRURL, r.NeedsInfoLabel = 0, "", "delamain:needs-info"
	assertMessage(t, FormatNeedsInfo(r), Message{
		Event:   EventNeedsInfo,
		Title:   "ℹ️ Needs info · delamain #12",
		Body:    "Mostrar HARNESS e INTERACTIONS no status\nIssue relabeled delamain:needs-info",
		Urgency: Low, Expire: 15000, Category: "x-delamain.needs-info",
		Actions: []Action{
			{Key: "default", Label: "Open issue", Target: sampleIssue},
			{Key: "open-issue", Label: "Open issue", Target: sampleIssue},
		},
	})
}

func TestFormatShouldTruncateTitleBodyAndReasonWithEllipsis(t *testing.T) {
	r := sampleRun()
	r.Repo = "/src/" + strings.Repeat("r", 90)
	r.Title = strings.Repeat("t", 61)
	r.Reason = strings.Repeat("é", 121)
	m := FormatHalted(r)
	if want := "⛔ Halted · " + strings.Repeat("r", 68) + "…"; m.Title != want {
		t.Fatalf("title must be cut to 80 characters with …:\n got %q\nwant %q", m.Title, want)
	}
	lines := strings.Split(m.Body, "\n")
	if lines[0] != strings.Repeat("t", 59)+"…" {
		t.Fatalf("issue title line must be cut to 60 characters with …: %q", lines[0])
	}
	if lines[2] != strings.Repeat("é", 119)+"…" {
		t.Fatalf("reason must be cut to 120 characters with …: %q", lines[2])
	}

	r.Title, r.Reason = strings.Repeat("t", 60), ""
	if m := FormatHalted(r); m.Body != strings.Repeat("t", 60)+"\nrun 42 · implement · 2/3 iterations" {
		t.Fatalf("a 60-character title stays whole and an empty reason adds no line: %q", m.Body)
	}
}

func TestFormatNeedsHumanWithoutPRShouldOfferReportAndIssue(t *testing.T) {
	r := sampleRun()
	r.PRURL, r.Iterations, r.Tokens = "", 3, "1.2M"
	m := FormatNeedsHuman(r)
	if want := "Mostrar HARNESS e INTERACTIONS no status\nNo PR · 3/3 iterations · 1.2M tokens"; m.Body != want {
		t.Fatalf("body = %q, want %q", m.Body, want)
	}
	report := "/state/runs/delamain/42/report.md"
	want := []Action{
		{Key: "default", Label: "Open report", Target: report},
		{Key: "open-report", Label: "Open report", Target: report},
		{Key: "open-issue", Label: "Open issue", Target: sampleIssue},
	}
	if !reflect.DeepEqual(m.Actions, want) {
		t.Fatalf("actions = %+v, want %+v", m.Actions, want)
	}
}

func TestFormatTaskWithoutIssueShouldNotOfferOpenIssue(t *testing.T) {
	r := sampleRun()
	r.Ref, r.IssueURL, r.PRURL = "task:add-login", "", ""
	report := "/state/runs/delamain/42/report.md"
	for _, m := range []Message{FormatNeedsHuman(r), FormatHalted(r)} {
		want := []Action{
			{Key: "default", Label: "Open report", Target: report},
			{Key: "open-report", Label: "Open report", Target: report},
		}
		if !reflect.DeepEqual(m.Actions, want) {
			t.Errorf("%s: a task has no issue to open and defaults to the report: %+v", m.Event, m.Actions)
		}
	}
	if m := FormatNeedsInfo(r); m.Title != "ℹ️ Needs info · delamain task:add-login" || m.Body != r.Title || len(m.Actions) != 0 {
		t.Errorf("needs_info of a task: no relabel line and nothing to open: %+v", m)
	}
	if m := FormatStarted(r); len(m.Actions) != 0 {
		t.Errorf("started of a task has nothing to open: %+v", m.Actions)
	}
}

func TestFormatPRNumberShouldFallBackToPRWithoutNumber(t *testing.T) {
	r := sampleRun()
	r.PRURL = "https://gitlab.example.com/group/api/-/merge_requests/7"
	if m := FormatPROpened(r); !strings.HasPrefix(strings.Split(m.Body, "\n")[1], "PR #7 · ") {
		t.Fatalf("an MR URL gives its number: %q", m.Body)
	}
	r.PRURL = "https://forge.example.com/acme/api/pulls/new"
	if m := FormatPROpened(r); !strings.HasPrefix(strings.Split(m.Body, "\n")[1], "PR · ") {
		t.Fatalf("without a number the fact is just PR: %q", m.Body)
	}
}
