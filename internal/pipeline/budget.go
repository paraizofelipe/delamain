package pipeline

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/store"
)

const defaultBudgetInterval = 10 * time.Second

func (r *Runner) meter() budget.Meter {
	return budget.Meter{Store: r.Store, Cfg: r.Cfg.Budget, Sessions: r.Agents.Sessions}
}

// meterFor meters a run against its own per-issue budget, which its complexity may have changed.
func (r *Runner) meterFor(ru *run) budget.Meter {
	m := r.meter()
	m.Cfg.PerIssue = ru.perIssue
	return m
}

// checkBudget records the run's usage after a turn and stops the run at the hard limit.
// Accounting failures halt too: an unmeasured run would spend without a ceiling.
func (r *Runner) checkBudget(ctx context.Context, ru *run) error {
	s, err := r.meterFor(ru).Check(ctx, ru.rec.ID, filepath.Join(ru.dir, "sessions"), liveUsage(ru.agents))
	if err != nil {
		return haltf("contabilidade de tokens falhou: %v", err)
	}
	s = runStatus(ru, s)
	if s.Level == budget.Soft && ru.budget.Level < budget.Soft {
		ru.log.Warn("budget soft limit", "run", ru.rec.ID, "reason", s.Reason)
	}
	ru.budget, ru.res.Budget = s, s.Summary()
	r.reportMeta(ctx, ru)
	if s.Level == budget.Hard {
		return halt{s.Reason}
	}
	return nil
}

// runStatus names a review's own budget in its reasons: it is review.per_review, not per_issue.
func runStatus(ru *run, s budget.Status) budget.Status {
	if ru.rec.Kind == store.KindReview {
		s.Reason = strings.Replace(s.Reason, "budget da issue", "budget da review", 1)
	}
	return s
}

type watchResult struct {
	tripped bool
	status  budget.Status
}

// watchBudget meters the run while an agent turn is in flight. At the hard limit it aborts the
// turn so a long one cannot overspend unchecked.
// The returned stop waits for the watcher to exit before the caller touches run state again.
func (r *Runner) watchBudget(ctx context.Context, ru *run, agent Agent) func() watchResult {
	interval := r.BudgetInterval
	if interval <= 0 {
		interval = defaultBudgetInterval
	}
	agents := maps.Clone(ru.agents) // the run's map is not touched from this goroutine
	done, out := make(chan struct{}), make(chan watchResult, 1)
	go func() {
		var res watchResult
		defer func() { out <- res }()
		t := time.NewTicker(interval)
		defer t.Stop()
		aborted := false
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s, err := r.meterFor(ru).Check(ctx, ru.rec.ID, filepath.Join(ru.dir, "sessions"), liveUsage(agents))
			if err != nil || s.Level != budget.Hard {
				continue
			}
			res = watchResult{tripped: true, status: runStatus(ru, s)}
			if !aborted {
				if err := agent.Abort(); err != nil {
					ru.log.Warn("could not abort agent turn", "err", err)
				}
				aborted = true
			}
		}
	}()
	return func() watchResult {
		close(done)
		return <-out
	}
}

// liveUsage is what each running agent reported while its turn ran, keyed like the sessions dir (by role).
func liveUsage(agents map[string]Agent) map[string]store.Usage {
	live := make(map[string]store.Usage, len(agents))
	for role, a := range agents {
		live[role] = storeUsage(a.Spent())
	}
	return live
}

// reportMeta shows the run's progress on the display. Display only: failures are logged.
func (r *Runner) reportMeta(ctx context.Context, ru *run) {
	if ru.view == nil {
		return
	}
	tokens := map[string]string{
		"state":  string(ru.rec.State),
		"iter":   fmt.Sprintf("%d/%d", ru.rec.Iteration, ru.maxIter),
		"tokens": budget.Format(ru.budget.Run, ru.budget.Mode),
	}
	if ru.budget.PerIssue > 0 {
		tokens["tokens"] += "/" + budget.Format(ru.budget.PerIssue, ru.budget.Mode)
	}
	ru.viewDo("metadata", func(v View) error { return v.Meta(ctx, tokens) })
}
