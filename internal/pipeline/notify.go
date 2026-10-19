package pipeline

import (
	"context"
	"path/filepath"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/notify"
)

// notify shows a desktop notification when its event is enabled. A failure never stops the run.
func (r *Runner) notify(ctx context.Context, m notify.Message) {
	if r.Notify == nil || !notify.Allowed(r.Cfg.Notify, m.Event) {
		return
	}
	if r.Forge != nil {
		m.ForgeHost = r.Forge.Repo().Host
	}
	if err := r.Notify.Notify(ctx, m); err != nil {
		r.Log.Warn("could not notify", "event", m.Event, "err", err)
	}
}

// notifyFinish tells the desktop how the run ended, once the issue already says so.
// Halted wins over a draft PR; like announce, only a ready PR is "opened".
func (r *Runner) notifyFinish(ctx context.Context, ru *run, t Task) {
	if ru.recovered {
		return // Recover shows one toast for all the runs it finalized
	}
	n := r.toast(ru, t)
	switch {
	case ru.res.Outcome == OutcomeHalted:
		r.notify(ctx, notify.FormatHalted(n))
	case ru.res.PRURL != "" && !ru.res.Draft:
		r.notify(ctx, notify.FormatPROpened(n))
	default:
		r.notify(ctx, notify.FormatNeedsHuman(n))
	}
}

// toast is what the run toasts show about ru.
func (r *Runner) toast(ru *run, t Task) notify.Run {
	res := ru.res
	return notify.Run{
		ID: ru.rec.ID, Repo: ru.rec.Repo, Ref: t.Ref, Title: t.Title, IssueURL: t.URL,
		PRURL: res.PRURL, Draft: res.Draft, Iterations: res.Iterations, MaxIterations: ru.maxIter,
		// Tokens whatever the budget mode: the toast reads like the issue comment.
		Tokens:  budget.Format(float64(ru.budget.Usage.BudgetTokens()), config.BudgetTokens),
		CostUSD: ru.budget.Usage.CostUSD,
		State:   string(ru.rec.State), Reason: res.Reason, Report: filepath.Join(ru.dir, "report.md"),
	}
}
