package notify

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Run is what a run's toasts show.
type Run struct {
	ID                        int64
	Repo                      string // repository path; the toast shows its base name
	Ref                       string // "#12" or "task:<slug>"
	Title                     string
	Harness                   string
	State                     string
	Iterations, MaxIterations int
	Tokens                    string // budget.Format of the run's tokens, e.g. "167K"
	CostUSD                   float64
	PRURL                     string
	Draft                     bool
	IssueURL                  string
	ReportPath                string
	Reason                    string
	NeedsInfoLabel            string
}

const (
	maxTitle  = 80
	maxLine   = 60
	maxReason = 120
	expire    = 15000
)

// FormatPROpened is the toast of an approved run delivered as a ready PR.
func FormatPROpened(r Run) Message {
	facts := []string{prLabel(r.PRURL), fmt.Sprintf("%d iterations", r.Iterations), r.Tokens + " tokens"}
	if r.CostUSD > 0 {
		facts = append(facts, fmt.Sprintf("US$ %.2f", r.CostUSD))
	}
	return Message{
		Event: EventPROpened, RunID: r.ID,
		Title:   title("✅ PR opened", r),
		Body:    body(r.Title, strings.Join(facts, " · "), ""),
		Urgency: Normal, Expire: expire, Category: "x-delamain.pr",
		Actions: actions(open("default", "Open PR", r.PRURL), open("open-pr", "Open PR", r.PRURL), open("open-issue", "Open issue", r.IssueURL)),
	}
}

// FormatStarted is the toast of a run that just got its record.
func FormatStarted(r Run) Message {
	return Message{
		Event: EventStarted, RunID: r.ID,
		Title:   title("▶️ Started", r),
		Body:    body(r.Title, fmt.Sprintf("run %d · %s", r.ID, r.Harness), ""),
		Urgency: Low, Expire: expire, Category: "x-delamain.started",
		Actions: open("default", "Open issue", r.IssueURL),
	}
}

// FormatNeedsHuman is the toast of work a human must finish: a draft PR, or no PR at all.
func FormatNeedsHuman(r Run) Message {
	pr := "No PR"
	if r.PRURL != "" {
		pr = prLabel(r.PRURL)
		if r.Draft {
			pr = "Draft " + pr
		}
	}
	facts := fmt.Sprintf("%s · %d/%d iterations · %s tokens", pr, r.Iterations, r.MaxIterations, r.Tokens)
	m := Message{
		Event: EventNeedsHuman, RunID: r.ID,
		Title:   title("⚠️ Needs review", r),
		Body:    body(r.Title, facts, r.Reason),
		Urgency: Normal, Expire: expire, Category: "x-delamain.needs-human",
	}
	if r.PRURL != "" {
		m.Actions = actions(open("default", "Open PR", r.PRURL), open("open-pr", "Open PR", r.PRURL), open("open-report", "Open report", r.ReportPath))
	} else {
		m.Actions = actions(open("default", "Open report", r.ReportPath), open("open-report", "Open report", r.ReportPath), open("open-issue", "Open issue", r.IssueURL))
	}
	return m
}

// FormatHalted is the toast of a run stopped by a guardrail or a failure.
func FormatHalted(r Run) Message {
	facts := fmt.Sprintf("run %d · %s · %d/%d iterations", r.ID, r.State, r.Iterations, r.MaxIterations)
	return Message{
		Event: EventHalted, RunID: r.ID,
		Title:   title("⛔ Halted", r),
		Body:    body(r.Title, facts, r.Reason),
		Urgency: Critical, Expire: 0, Category: "x-delamain.halted",
		Actions: actions(open("default", "Open report", r.ReportPath), open("open-report", "Open report", r.ReportPath), open("open-issue", "Open issue", r.IssueURL)),
	}
}

// FormatNeedsInfo is the toast of a task preflight rejected; a local task has no issue to relabel.
func FormatNeedsInfo(r Run) Message {
	facts := ""
	if r.IssueURL != "" {
		facts = "Issue relabeled " + r.NeedsInfoLabel
	}
	return Message{
		Event: EventNeedsInfo, RunID: r.ID,
		Title:   title("ℹ️ Needs info", r),
		Body:    body(r.Title, facts, ""),
		Urgency: Low, Expire: expire, Category: "x-delamain.needs-info",
		Actions: actions(open("default", "Open issue", r.IssueURL), open("open-issue", "Open issue", r.IssueURL)),
	}
}

func title(prefix string, r Run) string {
	return truncate(fmt.Sprintf("%s · %s %s", prefix, filepath.Base(r.Repo), r.Ref), maxTitle)
}

// body joins the task title, the facts line and the reason, dropping empty lines.
func body(taskTitle, facts, reason string) string {
	lines := []string{truncate(taskTitle, maxLine)}
	if facts != "" {
		lines = append(lines, facts)
	}
	if reason != "" {
		lines = append(lines, truncate(reason, maxReason))
	}
	return strings.Join(lines, "\n")
}

var prNumber = regexp.MustCompile(`/(\d+)/?$`)

// prLabel is "PR #<n>" from the last numeric segment of the PR/MR URL, or "PR" without one.
func prLabel(url string) string {
	if m := prNumber.FindStringSubmatch(url); m != nil {
		return "PR #" + m[1]
	}
	return "PR"
}

// open is an action, or nothing when its target is unknown.
func open(key, label, target string) []Action {
	if target == "" {
		return nil
	}
	return []Action{{Key: key, Label: label, Target: target}}
}

func actions(groups ...[]Action) []Action {
	var out []Action
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// truncate keeps at most n characters, the last one being "…" when s is cut.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
