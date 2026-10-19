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

func title(prefix string, r Run) string {
	return truncate(fmt.Sprintf("%s · %s %s", prefix, filepath.Base(r.Repo), r.Ref), maxTitle)
}

// body joins the task title, the facts line and the reason, dropping empty lines.
func body(taskTitle, facts, reason string) string {
	lines := []string{truncate(taskTitle, maxLine), facts}
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
