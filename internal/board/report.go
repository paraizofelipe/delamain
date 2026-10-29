package board

import (
	"net/url"
	"strconv"
	"time"
)

// ReportQuery is what the Relatório page asks for.
type ReportQuery struct {
	Period     string // 7d | 30d | 90d | all
	Repo, Kind string
	Offset     int // pagination of the history, 20 per page
}

// Report is the whole Relatório page, in one JSON.
type Report struct {
	Period      string          `json:"period"`
	Since       *time.Time      `json:"since,omitempty"` // nil in "all"
	Repos       []string        `json:"repos"`           // options of the filter
	KPIs        ReportKPIs      `json:"kpis"`
	Daily       []ReportDay     `json:"daily"`
	TopRuns     []ReportRun     `json:"top_runs"`     // up to 5, highest cost first
	TopRoles    []ReportRole    `json:"top_roles"`    // highest cost first
	TopSessions []ReportSession `json:"top_sessions"` // up to 10, highest cost first
	History     []ReportRun     `json:"history"`      // most recent first
	NextOffset  *int            `json:"next_offset"`  // nil when there is no more
}

// ReportKPIs are the totals of a period.
type ReportKPIs struct {
	Runs          int         `json:"runs"`
	CostUSD       float64     `json:"cost_usd"`
	AvgCostUSD    float64     `json:"avg_cost_usd"`
	MedianSeconds int64       `json:"median_seconds"`
	P90Seconds    int64       `json:"p90_seconds"`
	Approved      int         `json:"approved"`
	Halted        int         `json:"halted"`
	Previous      *ReportKPIs `json:"previous,omitempty"` // previous period of the same length; nil in "all"
}

// ReportDay is the spend of one day, per role.
type ReportDay struct {
	Day    string             `json:"day"`     // 2006-01-02, local time zone
	ByRole map[string]float64 `json:"by_role"` // cost in US$
}

// ReportRun is a run in the report.
type ReportRun struct {
	ID         int64           `json:"id"`
	Ref        string          `json:"ref"`
	Title      string          `json:"title"`
	Repo       string          `json:"repo"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	Outcome    string          `json:"outcome,omitempty"` // approved, changes, needs_info, halted…
	Iterations int             `json:"iterations"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"` // nil while it runs
	Seconds    int64           `json:"seconds"`
	CostUSD    float64         `json:"cost_usd"`
	Tokens     int64           `json:"tokens"`
	Sessions   []ReportSession `json:"sessions,omitempty"` // only in history
}

// ReportSession is one agent session of a run.
type ReportSession struct {
	RunID     int64      `json:"run_id"`
	RunRef    string     `json:"run_ref,omitempty"` // only in top_sessions
	Role      string     `json:"role"`
	Iteration int        `json:"iteration"` // 0 = outside the loop (planner) or unknown
	Model     string     `json:"model,omitempty"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Seconds   int64      `json:"seconds"`
	CostUSD   float64    `json:"cost_usd"`
	Tokens    int64      `json:"tokens"`
}

// ReportRole is the spend of one role over the period.
type ReportRole struct {
	Role     string  `json:"role"`
	Sessions int     `json:"sessions"`
	Seconds  int64   `json:"seconds"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   int64   `json:"tokens"`
}

// ParseReportQuery reads the query string of GET /api/report.
func ParseReportQuery(v url.Values) (ReportQuery, error) {
	q := ReportQuery{Period: v.Get("period"), Repo: v.Get("repo"), Kind: v.Get("kind")}
	if q.Period == "" {
		q.Period = "30d"
	}
	q.Offset, _ = strconv.Atoi(v.Get("offset"))
	return q, nil
}
