package cli

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/store"
)

// periodDays is the length of each period of the report; "all" has none.
var periodDays = map[string]int{"7d": 7, "30d": 30, "90d": 90}

// buildReport assembles the Relatório page from the store as of now.
func buildReport(ctx context.Context, st *store.Store, q board.ReportQuery, now time.Time) (board.Report, error) {
	rep := board.Report{
		Period: q.Period, Repos: []string{}, Daily: []board.ReportDay{}, TopRuns: []board.ReportRun{},
		TopRoles: []board.ReportRole{}, TopSessions: []board.ReportSession{}, History: []board.ReportRun{},
	}
	n := periodDays[q.Period]
	var since time.Time
	f := store.ReportFilter{Repo: q.Repo, Kind: q.Kind}
	if n > 0 {
		since = now.AddDate(0, 0, -n)
		rep.Since = &since
		// One query covers the previous period too: [since-n, since).
		f.Since = since.AddDate(0, 0, -n)
	}
	sums, err := st.RunSummaries(ctx, f)
	if err != nil {
		return board.Report{}, err
	}
	var prev []board.ReportRun
	for _, s := range sums {
		if s.Run.CreatedAt.Before(since) {
			prev = append(prev, reportRun(s, now))
			continue
		}
		rep.History = append(rep.History, reportRun(s, now))
	}
	rep.KPIs = reportKPIs(rep.History)
	rep.TopRuns = topByCost(rep.History, 5, func(r board.ReportRun) float64 { return r.CostUSD })
	if n > 0 {
		p := reportKPIs(prev)
		rep.KPIs.Previous = &p
	}
	return rep, nil
}

// topByCost is up to n of xs, highest cost first; a tie keeps their order.
func topByCost[T any](xs []T, n int, cost func(T) float64) []T {
	out := slices.Clone(xs)
	slices.SortStableFunc(out, func(a, b T) int { return cmp.Compare(cost(b), cost(a)) })
	return out[:min(n, len(out))]
}

// reportKPIs totals runs; the median and p90 durations are of the finished ones only.
func reportKPIs(runs []board.ReportRun) board.ReportKPIs {
	k := board.ReportKPIs{Runs: len(runs)}
	var secs []int64
	for _, r := range runs {
		k.CostUSD += r.CostUSD
		switch r.Outcome {
		case "approved":
			k.Approved++
		case "halted":
			k.Halted++
		}
		if r.EndedAt != nil {
			secs = append(secs, r.Seconds)
		}
	}
	if k.Runs > 0 {
		k.AvgCostUSD = k.CostUSD / float64(k.Runs)
	}
	if len(secs) > 0 {
		slices.Sort(secs)
		m := len(secs) / 2
		k.MedianSeconds = secs[m]
		if len(secs)%2 == 0 {
			k.MedianSeconds = (secs[m-1] + secs[m]) / 2
		}
		// Nearest rank: the ceil(0.9n)-th of the sorted durations.
		k.P90Seconds = secs[(9*len(secs)+9)/10-1]
	}
	return k
}

// reportRun is a run of the report; a run that has not finished runs until now.
func reportRun(s store.RunSummary, now time.Time) board.ReportRun {
	r := s.Run
	out := board.ReportRun{
		ID: r.ID, Ref: r.IssueRef, Title: r.Title, Repo: r.Repo, Kind: string(r.Kind), State: string(r.State),
		Iterations: r.Iteration, StartedAt: r.CreatedAt, CostUSD: s.Usage.CostUSD, Tokens: s.Usage.BudgetTokens(),
	}
	end := now
	if reportTerminal(r.State) {
		out.Outcome = board.Outcome(r.Reason)
		if out.Outcome == "" {
			out.Outcome = string(r.State)
		}
		end = r.UpdatedAt
		out.EndedAt = &end
	}
	out.Seconds = seconds(r.CreatedAt, end)
	return out
}

// reportTerminal is board's terminal: halted counts, unlike store.RunState.Terminal.
func reportTerminal(s store.RunState) bool {
	return s == store.StateRecorded || s == store.StateNeedsInfo || s == store.StateHalted
}

// seconds is the whole seconds from start to end, never negative.
func seconds(start, end time.Time) int64 {
	return max(int64(end.Sub(start)/time.Second), 0)
}
