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
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+`, COALESCE(sum_input,0), COALESCE(sum_output,0), COALESCE(sum_cache_read,0), COALESCE(sum_cache_write,0), COALESCE(sum_cost_usd,0)
		 FROM runs LEFT JOIN (
		   SELECT run_id, SUM(input) AS sum_input, SUM(output) AS sum_output, SUM(cache_read) AS sum_cache_read,
		     SUM(cache_write) AS sum_cache_write, SUM(cost_usd) AS sum_cost_usd
		   FROM token_ledger GROUP BY run_id
		 ) ON run_id = id
		 WHERE (?1 IS NULL OR runs.created_at >= ?1) AND `+repoKindFilter+`
		 ORDER BY created_at DESC, id DESC`, f.args()...)
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
	rows, err := s.db.QueryContext(ctx, `SELECT token_ledger.at, token_ledger.role, token_ledger.cost_usd FROM `+ledgerCut, f.args()...)
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

// CostByRole sums, per role, the same ledger rows DailyCostByRole does.
func (s *Store) CostByRole(ctx context.Context, f ReportFilter) (map[string]Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_ledger.role, SUM(token_ledger.input), SUM(token_ledger.output), SUM(token_ledger.cache_read), SUM(token_ledger.cache_write), SUM(token_ledger.cost_usd)
		 FROM `+ledgerCut+` GROUP BY token_ledger.role`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Usage{}
	for rows.Next() {
		var role string
		var u Usage
		if err := rows.Scan(&role, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			return nil, err
		}
		out[role] = u
	}
	return out, rows.Err()
}

// repoKindFilter keeps the rows of runs in the filter's repo (?2) and kind (?3); an empty one keeps all.
const repoKindFilter = `(?2 = '' OR runs.repo = ?2) AND (?3 = '' OR runs.kind = ?3)`

// ledgerCut is the FROM and WHERE of the ledger rows a ReportFilter selects. The rows of a deleted run
// have no runs columns: a repo or kind filter drops them, and without one they count, as in the daily budget.
const ledgerCut = `token_ledger LEFT JOIN runs ON runs.id = token_ledger.run_id
	WHERE (?1 IS NULL OR token_ledger.at >= ?1) AND ` + repoKindFilter

// args binds f to ?1 (Since, NULL when zero), ?2 (Repo) and ?3 (Kind). Since goes in UTC, as the
// store writes it: SQLite compares the timestamps as text.
func (f ReportFilter) args() []any {
	var since any
	if !f.Since.IsZero() {
		since = f.Since.UTC()
	}
	return []any{since, f.Repo, f.Kind}
}

// withUsage scans a run row followed by the five ledger sums into u, so scanRun reads the run columns.
type withUsage struct {
	row interface{ Scan(...any) error }
	u   *Usage
}

func (w withUsage) Scan(dest ...any) error {
	return w.row.Scan(append(dest, &w.u.Input, &w.u.Output, &w.u.CacheRead, &w.u.CacheWrite, &w.u.CostUSD)...)
}
