package cli

import (
	"context"
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
	f := store.ReportFilter{Repo: q.Repo, Kind: q.Kind}
	if n := periodDays[q.Period]; n > 0 {
		f.Since = now.AddDate(0, 0, -n)
		since := f.Since
		rep.Since = &since
	}
	sums, err := st.RunSummaries(ctx, f)
	if err != nil {
		return board.Report{}, err
	}
	for _, s := range sums {
		rep.History = append(rep.History, reportRun(s, now))
	}
	return rep, nil
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
