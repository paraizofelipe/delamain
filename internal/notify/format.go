package notify

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Run is what the run toasts show; the pipeline fills it from the run's result.
type Run struct {
	ID            int64
	Repo          string // repository path; the toast shows its base name
	Ref           string // "#12" or "task:<slug>"
	Title         string
	IssueURL      string // empty for local tasks
	PRURL         string
	Draft         bool
	Iterations    int
	MaxIterations int
	Tokens        string // the run's tokens as budget.Format renders them, e.g. "167K"
	CostUSD       float64
	State         string // the stage the run was in, e.g. "implement"
	Reason        string
	Report        string // absolute path of the run's report.md
}

const expire = 15000 // ms a non-critical toast stays up

// FormatPROpened is the toast of a run that opened a ready PR/MR.
func FormatPROpened(r Run) Message {
	facts := []string{prLabel(r.PRURL), fmt.Sprintf("%d iterations", r.Iterations), r.Tokens + " tokens"}
	if r.CostUSD > 0 {
		facts = append(facts, fmt.Sprintf("US$ %.2f", r.CostUSD))
	}
	actions := []Action{{"default", "Open PR", r.PRURL}, {"open-pr", "Open PR", r.PRURL}}
	if r.IssueURL != "" {
		actions = append(actions, Action{"open-issue", "Open issue", r.IssueURL})
	}
	return Message{
		Event: EventPROpened, RunID: r.ID,
		Title:   "✅ PR opened · " + subject(r),
		Body:    lines(r.Title, strings.Join(facts, " · ")),
		Urgency: Normal, Expire: expire, Category: "x-delamain.pr",
		Actions: actions,
	}
}

// subject is "<repo> <ref>", e.g. "delamain #12".
func subject(r Run) string { return filepath.Base(r.Repo) + " " + r.Ref }

// prLabel is "PR #<n>", n being the last numeric segment of the PR/MR URL.
func prLabel(url string) string {
	segs := strings.Split(strings.TrimRight(url, "/"), "/")
	return "PR #" + segs[len(segs)-1]
}

func lines(l ...string) string { return strings.Join(l, "\n") }

// FormatNeedsHuman is the toast of a run that ended with a draft PR or no PR at all.
func FormatNeedsHuman(r Run) Message {
	pr := "Draft " + prLabel(r.PRURL)
	facts := []string{pr, iterations(r), r.Tokens + " tokens"}
	return Message{
		Event: EventNeedsHuman, RunID: r.ID,
		Title:   "⚠️ Needs review · " + subject(r),
		Body:    lines(r.Title, strings.Join(facts, " · "), r.Reason),
		Urgency: Normal, Expire: expire, Category: "x-delamain.needs-human",
		Actions: []Action{{"default", "Open PR", r.PRURL}, {"open-pr", "Open PR", r.PRURL}, {"open-report", "Open report", r.Report}},
	}
}

// FormatHalted is the toast of a run stopped by a guardrail or a failure.
func FormatHalted(r Run) Message {
	facts := []string{fmt.Sprintf("run %d", r.ID), r.State, iterations(r)}
	actions := []Action{{"default", "Open report", r.Report}, {"open-report", "Open report", r.Report}}
	if r.IssueURL != "" {
		actions = append(actions, Action{"open-issue", "Open issue", r.IssueURL})
	}
	return Message{
		Event: EventHalted, RunID: r.ID,
		Title:   "⛔ Halted · " + subject(r),
		Body:    lines(r.Title, strings.Join(facts, " · "), r.Reason),
		Urgency: Critical, Category: "x-delamain.halted",
		Actions: actions,
	}
}

// iterations is "<i>/<max> iterations".
func iterations(r Run) string { return fmt.Sprintf("%d/%d iterations", r.Iterations, r.MaxIterations) }
