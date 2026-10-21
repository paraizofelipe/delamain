package notify

import (
	"fmt"
	"strings"

	"github.com/paraizo/delamain/internal/config"
)

// exampleRun is the run of the validated examples shown by `delamain notify --test`.
var exampleRun = Run{
	ID: 42, Repo: "delamain", Ref: "#12", Title: "Mostrar HARNESS e INTERACTIONS no status", Harness: "omp",
	State: "implement", Iterations: 2, MaxIterations: 3, Tokens: "167K", CostUSD: 0.67,
	PRURL: "https://github.com/paraizofelipe/delamain/pull/31", NeedsInfoLabel: "delamain:needs-info",
	IssueURL: "https://github.com/paraizofelipe/delamain/issues/12", Reason: "reviewer alterou a worktree",
}

// Sample is the example toast of an event, without actions.
func Sample(e Event) (Message, error) {
	r := exampleRun
	var m Message
	switch e {
	case EventStarted:
		m = FormatStarted(r)
	case EventIteration:
		r.MaxIterations, r.Tokens, r.Reason = 12, "1.2M", "review: 3 blocking findings (spec, security)"
		m = FormatIteration(r)
	case EventPROpened:
		m = FormatPROpened(r)
	case EventNeedsHuman:
		r.Draft, r.Iterations, r.Tokens, r.Reason = true, 3, "1.2M", "3 iterações sem aprovação"
		m = FormatNeedsHuman(r)
	case EventHalted:
		m = FormatHalted(r)
	case EventNeedsInfo:
		m = FormatNeedsInfo(r)
	case EventBudget:
		m = FormatBudget(Budget{Percent: 80, Used: "8.1M", Limit: "10M", CostUSD: 32.40})
	case EventRecover:
		m = FormatRecover([]Recovered{{Repo: "delamain", Ref: "#12", Result: "halted"}, {Repo: "api", Ref: "#7", Result: "draft PR"}})
	default:
		return Message{}, fmt.Errorf("unknown event %q (one of %s)", e, strings.Join(config.NotifyEvents, "|"))
	}
	m.Actions = nil
	return m, nil
}
