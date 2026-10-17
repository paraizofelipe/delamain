package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/paraizo/delamain/internal/budget"
)

const defaultBudgetInterval = 10 * time.Second

func (r *Runner) meter() budget.Meter { return budget.Meter{Store: r.Store, Cfg: r.Cfg.Budget} }

// checkBudget records the run's usage after a turn and stops the run at the hard limit.
// Accounting failures halt too: an unmeasured run would spend without a ceiling.
func (r *Runner) checkBudget(ctx context.Context, ru *run) error {
	s, err := r.meter().Check(ctx, ru.rec.ID, filepath.Join(ru.dir, "sessions"))
	if err != nil {
		return haltf("contabilidade de tokens falhou: %v", err)
	}
	if s.Level == budget.Soft && ru.budget.Level < budget.Soft {
		r.Log.Warn("budget soft limit", "run", ru.rec.ID, "reason", s.Reason)
	}
	ru.budget, ru.res.Budget = s, s.Summary()
	r.reportMeta(ctx, ru)
	if s.Level == budget.Hard {
		return halt{s.Reason}
	}
	return nil
}

type watchResult struct {
	tripped bool
	status  budget.Status
}

// watchBudget meters the run while an agent turn is in flight. At the hard limit it interrupts
// the agent (esc, then ctrl+c on the next tick) so a long turn cannot overspend unchecked.
// The returned stop waits for the watcher to exit before the caller touches run state again.
func (r *Runner) watchBudget(ctx context.Context, ru *run, agent string) func() watchResult {
	interval := r.BudgetInterval
	if interval <= 0 {
		interval = defaultBudgetInterval
	}
	done, out := make(chan struct{}), make(chan watchResult, 1)
	go func() {
		var res watchResult
		defer func() { out <- res }()
		t := time.NewTicker(interval)
		defer t.Stop()
		sent := 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			s, err := r.meter().Check(ctx, ru.rec.ID, filepath.Join(ru.dir, "sessions"))
			if err != nil || s.Level != budget.Hard {
				continue
			}
			res = watchResult{tripped: true, status: s}
			if sent < 2 {
				key := [...]string{"esc", "ctrl+c"}[sent]
				if err := r.Herdr.AgentSendKeys(ctx, agent, key); err != nil {
					r.Log.Warn("could not interrupt agent", "agent", agent, "err", err)
				}
				sent++
			}
		}
	}()
	return func() watchResult {
		close(done)
		return <-out
	}
}

// reportMeta shows the run's progress in herdr's sidebar. Display only: failures are logged.
func (r *Runner) reportMeta(ctx context.Context, ru *run) {
	if ru.workspace == "" {
		return
	}
	ru.metaSeq = max(ru.metaSeq+1, time.Now().UnixMilli()) // herdr drops non-increasing seqs
	tokens := map[string]string{
		"state":  string(ru.rec.State),
		"iter":   fmt.Sprintf("%d/%d", ru.rec.Iteration, ru.maxIter),
		"tokens": budget.Format(ru.budget.Run, ru.budget.Mode),
	}
	if ru.budget.PerIssue > 0 {
		tokens["tokens"] += "/" + budget.Format(ru.budget.PerIssue, ru.budget.Mode)
	}
	if err := r.Herdr.WorkspaceReportMetadata(ctx, ru.workspace, "delamain", ru.metaSeq, tokens); err != nil {
		r.Log.Debug("could not report workspace metadata", "err", err)
	}
}
