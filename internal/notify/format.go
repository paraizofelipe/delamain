package notify

import (
	"fmt"
	"path/filepath"
	"strconv"
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
	Harness       string
	Label         string // the needs_info label the issue got; empty when no issue was relabeled
}

const (
	expire = 15000 // ms a non-critical toast stays up

	// Limits in runes, the cut ones ending with "…".
	maxTitle  = 80
	maxLine1  = 60
	maxReason = 120
)

// FormatStarted is the toast of a run that just started (off by default).
func FormatStarted(r Run) Message {
	var actions []Action
	if r.IssueURL != "" {
		actions = []Action{{"default", "Open issue", r.IssueURL}}
	}
	return Message{
		Event: EventStarted, RunID: r.ID,
		Title:   toastTitle("▶️ Started · " + subject(r)),
		Body:    body(r.Title, fmt.Sprintf("run %d · %s", r.ID, r.Harness), ""),
		Urgency: Low, Expire: expire, Category: "x-delamain.started",
		Actions: actions,
	}
}

// FormatPROpened is the toast of a run that opened a ready PR/MR.
func FormatPROpened(r Run) Message {
	facts := []string{prLabel(r.PRURL), fmt.Sprintf("%d iterations", r.Iterations), r.Tokens + " tokens"}
	if r.CostUSD > 0 {
		facts = append(facts, fmt.Sprintf("US$ %.2f", r.CostUSD))
	}
	actions := []Action{{"default", "Open PR", r.PRURL}, {"open-pr", "Open PR", r.PRURL}}
	return Message{
		Event: EventPROpened, RunID: r.ID,
		Title:   toastTitle("✅ PR opened · " + subject(r)),
		Body:    body(r.Title, strings.Join(facts, " · "), ""),
		Urgency: Normal, Expire: expire, Category: "x-delamain.pr",
		Actions: withIssue(actions, r),
	}
}

// FormatNeedsHuman is the toast of a run that ended with a draft PR or no PR at all.
func FormatNeedsHuman(r Run) Message {
	pr, actions := "No PR", withIssue(reportActions(r), r)
	if r.PRURL != "" {
		pr = prLabel(r.PRURL)
		if r.Draft {
			pr = "Draft " + pr
		}
		actions = []Action{{"default", "Open PR", r.PRURL}, {"open-pr", "Open PR", r.PRURL}, {"open-report", "Open report", r.Report}}
	}
	return Message{
		Event: EventNeedsHuman, RunID: r.ID,
		Title:   toastTitle("⚠️ Needs review · " + subject(r)),
		Body:    body(r.Title, strings.Join([]string{pr, iterations(r), r.Tokens + " tokens"}, " · "), r.Reason),
		Urgency: Normal, Expire: expire, Category: "x-delamain.needs-human",
		Actions: actions,
	}
}

// FormatHalted is the toast of a run stopped by a guardrail or a failure.
func FormatHalted(r Run) Message {
	facts := []string{fmt.Sprintf("run %d", r.ID), r.State, iterations(r)}
	return Message{
		Event: EventHalted, RunID: r.ID,
		Title:   toastTitle("⛔ Halted · " + subject(r)),
		Body:    body(r.Title, strings.Join(facts, " · "), r.Reason),
		Urgency: Critical, Category: "x-delamain.halted",
		Actions: withIssue(reportActions(r), r),
	}
}

// FormatNeedsInfo is the toast of a task the preflight rejected; there is no run.
func FormatNeedsInfo(r Run) Message {
	relabeled := ""
	if r.Label != "" {
		relabeled = "Issue relabeled " + r.Label
	}
	var actions []Action
	if r.IssueURL != "" {
		actions = []Action{{"default", "Open issue", r.IssueURL}, {"open-issue", "Open issue", r.IssueURL}}
	}
	return Message{
		Event:   EventNeedsInfo,
		Title:   toastTitle("ℹ️ Needs info · " + subject(r)),
		Body:    body(r.Title, relabeled, ""),
		Urgency: Low, Expire: expire, Category: "x-delamain.needs-info",
		Actions: actions,
	}
}

func reportActions(r Run) []Action {
	return []Action{{"default", "Open report", r.Report}, {"open-report", "Open report", r.Report}}
}

// withIssue adds "Open issue" when the run has an issue (local tasks have none).
func withIssue(actions []Action, r Run) []Action {
	if r.IssueURL == "" {
		return actions
	}
	return append(actions, Action{"open-issue", "Open issue", r.IssueURL})
}

// subject is "<repo> <ref>", e.g. "delamain #12".
func subject(r Run) string { return filepath.Base(r.Repo) + " " + r.Ref }

// iterations is "<i>/<max> iterations".
func iterations(r Run) string { return fmt.Sprintf("%d/%d iterations", r.Iterations, r.MaxIterations) }

// prLabel is "PR #<n>", n being the last segment of the PR/MR URL, or "PR" when it is not a number.
func prLabel(url string) string {
	segs := strings.Split(strings.TrimRight(url, "/"), "/")
	n := segs[len(segs)-1]
	if _, err := strconv.Atoi(n); err != nil {
		return "PR"
	}
	return "PR #" + n
}

func toastTitle(s string) string { return cut(s, maxTitle) }

// body is the issue/task title, the facts line and the reason, each line within its limit; empty
// lines are left out.
func body(title, facts, reason string) string {
	var out []string
	for _, l := range []string{cut(title, maxLine1), facts, cut(reason, maxReason)} {
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// cut keeps s within n runes, ending a cut one with "…".
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// Budget is the day's spend the budget toasts show.
type Budget struct {
	Hard bool
	Pct  int // the soft threshold crossed, e.g. 80
	// Used and Limit as budget.Format renders them: "8.1M" and "10M", or "US$ 8.10" in usd mode.
	Used, Limit string
	CostUSD     float64
	USD         bool
}

// FormatBudget is the toast of the daily budget reaching its soft or hard limit.
func FormatBudget(b Budget) Message {
	spend := b.Used + " of " + b.Limit
	if !b.USD {
		spend += " tokens"
		if b.CostUSD > 0 {
			spend += fmt.Sprintf(" · US$ %.2f", b.CostUSD)
		}
	}
	m := Message{
		Event: EventBudget, Title: fmt.Sprintf("💸 Daily budget at %d%%", b.Pct),
		Body:    lines(spend, "New runs continue until the hard limit"),
		Urgency: Normal, Expire: expire, Category: "x-delamain.budget",
	}
	if b.Hard {
		m.Title, m.Body, m.Urgency, m.Expire = "🛑 Daily budget exhausted", lines(spend, "Daemon paused until 00:00"), Critical, 0
	}
	return m
}

func lines(l ...string) string { return strings.Join(l, "\n") }
