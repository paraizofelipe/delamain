package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/paraizo/delamain/internal/artifacts"
	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/store"
)

// notify shows a desktop toast. Like issueUpdate, a failure never touches the run.
func (r *Runner) notify(ctx context.Context, log *slog.Logger, m notify.Message) {
	if r.Notify == nil {
		return
	}
	if err := r.Notify.Notify(ctx, m); err != nil {
		log.Warn("desktop notification failed", "event", m.Event, "err", err)
	}
}

// notifyRun is what the toasts of ru show; state is where the run stood before delivery.
func notifyRun(ru *run, t Task, state store.RunState) notify.Run {
	n := notify.Run{
		ID: ru.rec.ID, Repo: ru.rec.Repo, Ref: t.Ref, Title: t.Title, Harness: ru.rec.Harness, State: string(state),
		Iterations: ru.res.Iterations, MaxIterations: ru.maxIter,
		Tokens:  budget.Format(float64(ru.budget.Usage.BudgetTokens()), config.BudgetTokens),
		CostUSD: ru.budget.Usage.CostUSD,
		PRURL:   ru.res.PRURL, Draft: ru.res.Draft, IssueURL: t.URL, Reason: ru.res.Reason,
	}
	if ru.dir != "" {
		n.ReportPath = filepath.Join(ru.dir, "report.md") // written by record, right before the toast
	}
	return n
}

// outcomeToast maps the run's outcome to its toast, as announce maps it to the issue label.
func outcomeToast(ru *run, t Task, state store.RunState) notify.Message {
	n := notifyRun(ru, t, state)
	switch {
	case ru.res.Outcome == OutcomeHalted:
		return notify.FormatHalted(n)
	case ru.res.PRURL != "" && !ru.res.Draft:
		return notify.FormatPROpened(n)
	default:
		return notify.FormatNeedsHuman(n)
	}
}

// iterationToast announces the iteration ru is starting; why is how the previous one failed.
func iterationToast(ru *run, t Task, why string) notify.Message {
	n := notifyRun(ru, t, ru.rec.State)
	n.Reason = why
	return notify.FormatIteration(n)
}

// reviewReason counts the review's blocking findings and names the axes that have one, in order.
func reviewReason(r artifacts.Review) string {
	count, axes := 0, []string{}
	for _, a := range r.Axes {
		k := 0
		for _, f := range a.Findings {
			if f.Blocking() {
				k++
			}
		}
		if k > 0 {
			count += k
			axes = append(axes, a.Axis)
		}
	}
	noun := "findings"
	if count == 1 {
		noun = "finding"
	}
	return fmt.Sprintf("review: %d blocking %s (%s)", count, noun, strings.Join(axes, ", "))
}
