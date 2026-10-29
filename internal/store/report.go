package store

import (
	"context"
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
