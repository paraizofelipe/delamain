package cli

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/store"
)

// periodDays is the length of each period of the report; "all" has none.
var periodDays = map[string]int{"7d": 7, "30d": 30, "90d": 90}

// historyPage is how many runs a page of the history has.
const historyPage = 20

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
	var runs, prev []board.ReportRun
	for _, s := range sums {
		if s.Run.CreatedAt.Before(since) {
			prev = append(prev, reportRun(s, now))
			continue
		}
		runs = append(runs, reportRun(s, now))
	}
	rep.KPIs = reportKPIs(runs)
	rep.TopRuns = topByCost(runs, 5, func(r board.ReportRun) float64 { return r.CostUSD })
	// An offset past the end is an empty page.
	from := min(q.Offset, len(runs))
	to := min(from+historyPage, len(runs))
	rep.History = append(rep.History, runs[from:to]...)
	if to < len(runs) {
		rep.NextOffset = &to
	}
	sessions, err := reportSessions(ctx, st, runs, now)
	if err != nil {
		return board.Report{}, err
	}
	for i := range rep.History {
		rep.History[i].Sessions = sessions[rep.History[i].ID]
	}
	var all []board.ReportSession
	for _, r := range runs {
		for _, s := range sessions[r.ID] {
			s.RunRef = r.Ref
			all = append(all, s)
		}
	}
	rep.TopSessions = topByCost(all, 10, func(s board.ReportSession) float64 { return s.CostUSD })
	rep.TopRoles = reportRoles(all)
	if n > 0 {
		p := reportKPIs(prev)
		rep.KPIs.Previous = &p
	}
	return rep, nil
}

// topByCost is up to n of xs, highest cost first, never nil; a tie keeps their order.
func topByCost[T any](xs []T, n int, cost func(T) float64) []T {
	out := append([]T{}, xs...)
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

// reportSessions is the sessions of runs, keyed by run, in start order. A run from before
// agent_sessions has one session per agents row, with what its role billed in the run.
func reportSessions(ctx context.Context, st *store.Store, runs []board.ReportRun, now time.Time) (map[int64][]board.ReportSession, error) {
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	recorded, err := st.Sessions(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[int64][]board.ReportSession{}
	for _, id := range ids {
		if ss, ok := recorded[id]; ok {
			for _, a := range ss {
				out[id] = append(out[id], reportSession(a, now))
			}
			continue
		}
		agents, err := st.Agents(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(agents) == 0 {
			continue
		}
		usage, err := st.RoleUsage(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, a := range agents {
			as := store.AgentSession{RunID: id, Role: a.Role, State: a.State, StartedAt: a.StartedAt, EndedAt: a.EndedAt, Usage: usage[a.Role]}
			out[id] = append(out[id], reportSession(as, now))
		}
	}
	return out, nil
}

// reportRoles sums sessions per role, highest cost first and, on a tie, by role.
func reportRoles(sessions []board.ReportSession) []board.ReportRole {
	idx := map[string]int{}
	out := []board.ReportRole{}
	for _, s := range sessions {
		i, ok := idx[s.Role]
		if !ok {
			i = len(out)
			idx[s.Role] = i
			out = append(out, board.ReportRole{Role: s.Role})
		}
		out[i].Sessions++
		out[i].Seconds += s.Seconds
		out[i].CostUSD += s.CostUSD
		out[i].Tokens += s.Tokens
	}
	slices.SortFunc(out, func(a, b board.ReportRole) int {
		if c := cmp.Compare(b.CostUSD, a.CostUSD); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
	return out
}

// reportSession is a session of the report; one still running runs until now.
func reportSession(a store.AgentSession, now time.Time) board.ReportSession {
	s := board.ReportSession{
		RunID: a.RunID, Role: a.Role, Iteration: a.Iteration, Model: a.Model, State: string(a.State), StartedAt: a.StartedAt,
		CostUSD: a.Usage.CostUSD, Tokens: a.Usage.BudgetTokens(),
	}
	if a.EndedAt.IsZero() {
		s.Seconds = seconds(a.StartedAt, now)
		return s
	}
	end := a.EndedAt
	s.EndedAt = &end
	s.Seconds = seconds(a.StartedAt, end)
	return s
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
