package cli

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/store"
)

// reportNow is noon, local time, so whole days back stay on the same hour of a local day.
var reportNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

// sql runs q straight on the fixture's store file, to write the timestamps Store.now would have.
// Times go in UTC, as the store writes them.
func (f *fixture) sql(t *testing.T, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.stateDir, "delamain.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, a := range args {
		if at, ok := a.(time.Time); ok {
			args[i] = at.UTC()
		}
	}
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// pastRun seeds a run of repo created at start and last updated at end.
func (f *fixture) pastRun(t *testing.T, repo, ref string, kind store.RunKind, state store.RunState, reason string, start, end time.Time) store.Run {
	t.Helper()
	ctx := context.Background()
	rec, err := f.st.CreateRun(ctx, repo, ref, "Fix "+ref, "omp", 3, 0, kind)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Reason, rec.Iteration = state, reason, 1
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	f.sql(t, `UPDATE runs SET created_at = ?, updated_at = ? WHERE id = ?`, start, end, rec.ID)
	rec.CreatedAt, rec.UpdatedAt = start, end
	return rec
}

// spend bills cost and tokens (as input) to role in run at at.
func (f *fixture) spend(t *testing.T, run int64, role string, at time.Time, cost float64, tokens int64) {
	t.Helper()
	f.sql(t, `INSERT INTO token_ledger (run_id, role, at, input, output, cache_read, cache_write, cost_usd) VALUES (?, ?, ?, ?, 0, 0, 0, ?)`,
		run, role, at, tokens, cost)
}

func report(t *testing.T, f *fixture, q board.ReportQuery) board.Report {
	t.Helper()
	rep, err := buildReport(context.Background(), f.st, q, reportNow)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestBoardReportShouldComputeKPIsAndThePreviousPeriod(t *testing.T) {
	f := newFixture(t)
	day := 24 * time.Hour
	run := func(ref string, state store.RunState, reason string, daysAgo int, secs int, cost float64) {
		start := reportNow.AddDate(0, 0, -daysAgo)
		r := f.pastRun(t, f.repo, ref, store.KindIssue, state, reason, start, start.Add(time.Duration(secs)*time.Second))
		if cost > 0 {
			f.spend(t, r.ID, "implementer", start, cost, 10)
		}
	}
	// The 7 days up to now.
	run("#1", store.StateRecorded, "approved: PR aberto", 5, 100, 1)
	run("#2", store.StateRecorded, "halted: budget", 4, 300, 2)
	run("#3", store.StateNeedsInfo, "", 3, 200, 0.5)
	run("#4", store.StateHalted, "", 2, 400, 0)
	active := f.pastRun(t, f.repo, "#5", store.KindIssue, store.StateImplement, "", reportNow.Add(-time.Hour), reportNow.Add(-time.Minute))
	f.spend(t, active.ID, "implementer", reportNow.Add(-time.Hour), 0.5, 10)
	// The 7 days before them.
	run("#6", store.StateRecorded, "approved: PR aberto", 10, 60, 3)
	run("#7", store.StateNeedsInfo, "needs_info: falta o critério", 8, 120, 1)
	// Older than both.
	run("#8", store.StateRecorded, "changes: ainda falta", 20, 500, 0.25)

	rep := report(t, f, board.ReportQuery{Period: "7d"})
	if want := reportNow.Add(-7 * day); rep.Since == nil || !rep.Since.Equal(want) {
		t.Errorf("since = %v, want %v", rep.Since, want)
	}
	got := rep.KPIs
	prev := got.Previous
	got.Previous = nil
	// Median and p90 only of the finished runs: 100, 200, 300, 400 seconds.
	want := board.ReportKPIs{Runs: 5, CostUSD: 4, AvgCostUSD: 0.8, MedianSeconds: 250, P90Seconds: 400, Approved: 1, Halted: 2}
	if got != want {
		t.Errorf("kpis = %+v, want %+v", got, want)
	}
	if prev == nil {
		t.Fatal("no previous period in 7d")
	}
	if want := (board.ReportKPIs{Runs: 2, CostUSD: 4, AvgCostUSD: 2, MedianSeconds: 90, P90Seconds: 120, Approved: 1}); *prev != want {
		t.Errorf("previous = %+v, want %+v", *prev, want)
	}

	all := report(t, f, board.ReportQuery{Period: "all"})
	if all.Since != nil || all.KPIs.Previous != nil {
		t.Errorf("all has no since nor previous: %v %+v", all.Since, all.KPIs.Previous)
	}
	// 60, 100, 120, 200, 300, 400, 500 seconds.
	want = board.ReportKPIs{Runs: 8, CostUSD: 8.25, AvgCostUSD: 8.25 / 8, MedianSeconds: 200, P90Seconds: 500, Approved: 2, Halted: 2}
	if all.KPIs != want {
		t.Errorf("kpis in all = %+v, want %+v", all.KPIs, want)
	}
}

// costlyRun seeds a finished run started hoursAgo that cost cost, billed to role.
func (f *fixture) costlyRun(t *testing.T, ref string, hoursAgo int, role string, cost float64) store.Run {
	t.Helper()
	start := reportNow.Add(-time.Duration(hoursAgo) * time.Hour)
	r := f.pastRun(t, f.repo, ref, store.KindIssue, store.StateRecorded, "approved: PR aberto", start, start.Add(time.Minute))
	f.spend(t, r.ID, role, start, cost, 10)
	return r
}

func TestBoardReportShouldRankTheMostExpensiveRuns(t *testing.T) {
	f := newFixture(t)
	for i, cost := range []float64{0.5, 3, 1, 2, 2, 0.1, 4} {
		f.costlyRun(t, "#"+strconv.Itoa(i+1), 100-10*i, "implementer", cost)
	}

	rep := report(t, f, board.ReportQuery{Period: "7d"})
	var got []string
	for _, r := range rep.TopRuns {
		got = append(got, r.Ref)
		if r.Sessions != nil {
			t.Errorf("%s: top_runs carry no sessions: %+v", r.Ref, r.Sessions)
		}
	}
	// A tie keeps the newest first, as in the history.
	if want := []string{"#7", "#2", "#5", "#4", "#3"}; !slices.Equal(got, want) {
		t.Errorf("top_runs = %v, want %v", got, want)
	}
}
