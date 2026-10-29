package store

import (
	"context"
	"slices"
	"strings"
	"time"
)

// ReportFilter narrows the report queries: a zero Since has no lower bound, an empty Repo or Kind keeps all.
type ReportFilter struct {
	Since      time.Time
	Repo, Kind string
}

// RunSummary is a run with the total its token_ledger rows bill.
type RunSummary struct {
	Run   Run
	Usage Usage
}

// RunSummaries lists the runs created since f.Since that match f, newest first, each with its ledger total.
func (s *Store) RunSummaries(ctx context.Context, f ReportFilter) ([]RunSummary, error) {
	where, args := reportWhere(f, "runs.created_at")
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+`, COALESCE(sum_input,0), COALESCE(sum_output,0), COALESCE(sum_cache_read,0), COALESCE(sum_cache_write,0), COALESCE(sum_cost_usd,0)
		 FROM runs LEFT JOIN (
		   SELECT run_id, SUM(input) AS sum_input, SUM(output) AS sum_output, SUM(cache_read) AS sum_cache_read,
		     SUM(cache_write) AS sum_cache_write, SUM(cost_usd) AS sum_cost_usd
		   FROM token_ledger GROUP BY run_id
		 ) ON run_id = id
		 WHERE `+where+`
		 ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunSummary
	for rows.Next() {
		var u Usage
		r, err := scanRun(withUsage{rows, &u})
		if err != nil {
			return nil, err
		}
		out = append(out, RunSummary{Run: r, Usage: u})
	}
	return out, rows.Err()
}

// DayCost is what a role cost on one local day; Day is that day's midnight in time.Local.
type DayCost struct {
	Day     time.Time
	Role    string
	CostUSD float64
}

// DailyCostByRole sums the cost of the ledger rows since f.Since per local day and role, ordered by day then role.
func (s *Store) DailyCostByRole(ctx context.Context, f ReportFilter) ([]DayCost, error) {
	from, args := ledgerFrom(f)
	rows, err := s.db.QueryContext(ctx, `SELECT token_ledger.at, token_ledger.role, token_ledger.cost_usd FROM `+from, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// The day is local, so it is computed here: SQLite's date() is UTC.
	type key struct {
		day  int64
		role string
	}
	idx := map[key]int{}
	var out []DayCost
	for rows.Next() {
		var at time.Time
		var role string
		var cost float64
		if err := rows.Scan(&at, &role, &cost); err != nil {
			return nil, err
		}
		local := at.In(time.Local)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
		k := key{day.Unix(), role}
		if i, ok := idx[k]; ok {
			out[i].CostUSD += cost
			continue
		}
		idx[k] = len(out)
		out = append(out, DayCost{Day: day, Role: role, CostUSD: cost})
	}
	slices.SortFunc(out, func(a, b DayCost) int {
		if c := a.Day.Compare(b.Day); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
	return out, rows.Err()
}

// ledgerFrom is the FROM and WHERE of the ledger rows f selects, with their arguments. Only a repo or
// kind filter joins with runs: without one, the rows of deleted runs count, as in the daily budget.
func ledgerFrom(f ReportFilter) (string, []any) {
	where, args := reportWhere(f, "token_ledger.at")
	if f.Repo == "" && f.Kind == "" {
		return `token_ledger WHERE ` + where, args
	}
	return `token_ledger JOIN runs ON runs.id = token_ledger.run_id WHERE ` + where, args
}

// reportWhere is the WHERE clause of f, with at as its period column, and its arguments.
// Since goes in UTC, as the store writes it: SQLite compares the timestamps as text.
func reportWhere(f ReportFilter, at string) (string, []any) {
	conds, args := []string{"1"}, []any{}
	if !f.Since.IsZero() {
		conds, args = append(conds, at+" >= ?"), append(args, f.Since.UTC())
	}
	if f.Repo != "" {
		conds, args = append(conds, "runs.repo = ?"), append(args, f.Repo)
	}
	if f.Kind != "" {
		conds, args = append(conds, "runs.kind = ?"), append(args, f.Kind)
	}
	return strings.Join(conds, " AND "), args
}

// withUsage scans a run row followed by the five ledger sums into u, so scanRun reads the run columns.
type withUsage struct {
	row interface{ Scan(...any) error }
	u   *Usage
}

func (w withUsage) Scan(dest ...any) error {
	return w.row.Scan(append(dest, &w.u.Input, &w.u.Output, &w.u.CacheRead, &w.u.CacheWrite, &w.u.CostUSD)...)
}
