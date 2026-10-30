package cli

import (
	"context"
	"database/sql"
	"maps"
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

func TestBoardReportShouldPageTheHistory(t *testing.T) {
	f := newFixture(t)
	for i := range 45 {
		start := reportNow.Add(-time.Duration(i+1) * time.Hour)
		f.pastRun(t, f.repo, "#"+strconv.Itoa(i+1), store.KindIssue, store.StateRecorded, "approved: ok", start, start.Add(time.Minute))
	}

	for _, c := range []struct {
		offset     int
		first      string
		size       int
		nextOffset int // 0: none
	}{
		{0, "#1", 20, 20},
		{20, "#21", 20, 40},
		{40, "#41", 5, 0},
		{45, "", 0, 0},
		{1000, "", 0, 0},
	} {
		rep := report(t, f, board.ReportQuery{Period: "30d", Offset: c.offset})
		if len(rep.History) != c.size || rep.History == nil {
			t.Errorf("offset %d: %d runs (nil: %v), want %d", c.offset, len(rep.History), rep.History == nil, c.size)
			continue
		}
		if c.size > 0 && rep.History[0].Ref != c.first {
			t.Errorf("offset %d starts at %s, want %s", c.offset, rep.History[0].Ref, c.first)
		}
		switch {
		case c.nextOffset == 0 && rep.NextOffset != nil:
			t.Errorf("offset %d: next_offset = %d, want none", c.offset, *rep.NextOffset)
		case c.nextOffset != 0 && (rep.NextOffset == nil || *rep.NextOffset != c.nextOffset):
			t.Errorf("offset %d: next_offset = %v, want %d", c.offset, rep.NextOffset, c.nextOffset)
		}
		if rep.KPIs.Runs != 45 {
			t.Errorf("offset %d: the kpis are of the whole period, got %d runs", c.offset, rep.KPIs.Runs)
		}
	}
}

// nullTime is t, or NULL when it is zero.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// session seeds an agent session of run; a zero end is one still running.
func (f *fixture) session(t *testing.T, run int64, role string, iteration int, model string, state store.AgentState, start, end time.Time) {
	t.Helper()
	f.sql(t, `INSERT INTO agent_sessions (run_id, role, iteration, model, state, started_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run, role, iteration, model, state, start, nullTime(end))
}

// agent seeds the agents row of role in run, all a run from before agent_sessions has.
func (f *fixture) agent(t *testing.T, run int64, role string, state store.AgentState, start, end time.Time) {
	t.Helper()
	f.sql(t, `INSERT INTO agents (run_id, role, state, started_at, ended_at) VALUES (?, ?, ?, ?, ?)`, run, role, state, start, nullTime(end))
}

func TestBoardReportShouldFallBackToAgentsForOldRuns(t *testing.T) {
	f := newFixture(t)
	sec := time.Second
	t0 := reportNow.Add(-2 * time.Hour)
	running := f.pastRun(t, f.repo, "#1", store.KindIssue, store.StateImplement, "", t0, t0.Add(300*sec))
	f.session(t, running.ID, "planner", 0, "m1", store.AgentDone, t0, t0.Add(60*sec))
	f.session(t, running.ID, "implementer", 1, "m2", store.AgentDone, t0.Add(60*sec), t0.Add(300*sec))
	f.session(t, running.ID, "implementer", 2, "m2", store.AgentRunning, t0.Add(300*sec), time.Time{})
	f.spend(t, running.ID, "planner", t0.Add(30*sec), 0.25, 100)
	f.spend(t, running.ID, "implementer", t0.Add(200*sec), 0.5, 200)
	f.spend(t, running.ID, "implementer", t0.Add(400*sec), 0.75, 300)

	t1 := reportNow.AddDate(0, 0, -3)
	old := f.pastRun(t, f.repo, "#2", store.KindIssue, store.StateRecorded, "approved: PR aberto", t1, t1.Add(300*sec))
	f.agent(t, old.ID, "planner", store.AgentDone, t1, t1.Add(50*sec))
	f.agent(t, old.ID, "implementer", store.AgentDone, t1.Add(50*sec), t1.Add(250*sec))
	f.spend(t, old.ID, "planner", t1.Add(10*sec), 0.125, 50)
	f.spend(t, old.ID, "implementer", t1.Add(60*sec), 0.25, 100)
	f.spend(t, old.ID, "implementer", t1.Add(200*sec), 0.125, 50)
	// A role with spend but no agents row has no session to show.
	f.spend(t, old.ID, "reviewer", t1.Add(260*sec), 0.0625, 5)

	rep := report(t, f, board.ReportQuery{Period: "7d"})
	if len(rep.History) != 2 {
		t.Fatalf("history = %+v", rep.History)
	}
	type seg struct {
		Role      string
		Iteration int
		Model     string
		State     string
		Ended     bool
		Seconds   int64
		CostUSD   float64
		Tokens    int64
	}
	segs := func(ss []board.ReportSession) []seg {
		var out []seg
		for _, s := range ss {
			if s.RunRef != "" {
				t.Errorf("run_ref only in top_sessions: %+v", s)
			}
			out = append(out, seg{s.Role, s.Iteration, s.Model, s.State, s.EndedAt != nil, s.Seconds, s.CostUSD, s.Tokens})
		}
		return out
	}
	want := []seg{
		{"planner", 0, "m1", "done", true, 60, 0.25, 100},
		{"implementer", 1, "m2", "done", true, 240, 0.5, 200},
		{"implementer", 2, "m2", "running", false, int64(reportNow.Sub(t0.Add(300*sec)) / sec), 0.75, 300},
	}
	if got := segs(rep.History[0].Sessions); !slices.Equal(got, want) {
		t.Errorf("sessions of the running run =\n%+v\nwant\n%+v", got, want)
	}
	want = []seg{
		{"planner", 0, "", "done", true, 50, 0.125, 50},
		{"implementer", 0, "", "done", true, 200, 0.375, 150},
	}
	if got := segs(rep.History[1].Sessions); !slices.Equal(got, want) {
		t.Errorf("sessions of the old run, one per role =\n%+v\nwant\n%+v", got, want)
	}
	for _, r := range rep.TopRuns {
		if r.Sessions != nil {
			t.Errorf("%s: top_runs carry no sessions", r.Ref)
		}
	}
}

func TestBoardReportShouldRankRolesAndSessionsByCost(t *testing.T) {
	f := newFixture(t)
	sec := time.Second
	// session seeds a 10s session of role in r that cost cost.
	session := func(r store.Run, role string, cost float64) {
		f.session(t, r.ID, role, 1, "m", store.AgentDone, r.CreatedAt, r.CreatedAt.Add(10*sec))
		f.spend(t, r.ID, role, r.CreatedAt.Add(5*sec), cost, 10)
	}
	var runs []store.Run
	for i := 1; i <= 22; i++ {
		start := reportNow.Add(-time.Duration(i) * time.Hour)
		runs = append(runs, f.pastRun(t, f.repo, "#"+strconv.Itoa(i), store.KindIssue, store.StateRecorded, "approved: ok", start, start.Add(time.Minute)))
	}
	for i, r := range runs[:21] {
		session(r, "implementer", float64(i+1)*0.25)
	}
	// The costliest session is in a run past the first page of the history.
	session(runs[21], "implementer", 9)
	session(runs[0], "planner", 4)
	session(runs[1], "reviewer", 0.5)
	start := reportNow.Add(-30 * time.Hour)
	old := f.pastRun(t, f.repo, "#23", store.KindIssue, store.StateRecorded, "approved: ok", start, start.Add(time.Minute))
	f.agent(t, old.ID, "analyzer", store.AgentDone, start, start.Add(20*sec))
	f.spend(t, old.ID, "analyzer", start.Add(sec), 0.5, 10)

	rep := report(t, f, board.ReportQuery{Period: "7d"})
	type top struct {
		Ref, Role string
		CostUSD   float64
	}
	var got []top
	for _, s := range rep.TopSessions {
		got = append(got, top{s.RunRef, s.Role, s.CostUSD})
	}
	// A tie keeps the newest run first.
	want := []top{
		{"#22", "implementer", 9}, {"#21", "implementer", 5.25}, {"#20", "implementer", 5}, {"#19", "implementer", 4.75},
		{"#18", "implementer", 4.5}, {"#17", "implementer", 4.25}, {"#1", "planner", 4}, {"#16", "implementer", 4},
		{"#15", "implementer", 3.75}, {"#14", "implementer", 3.5},
	}
	if !slices.Equal(got, want) {
		t.Errorf("top_sessions =\n%+v\nwant\n%+v", got, want)
	}
	// A tie of cost goes by the name of the role.
	wantRoles := []board.ReportRole{
		{Role: "implementer", Sessions: 22, Seconds: 220, CostUSD: 66.75, Tokens: 220},
		{Role: "planner", Sessions: 1, Seconds: 10, CostUSD: 4, Tokens: 10},
		{Role: "analyzer", Sessions: 1, Seconds: 20, CostUSD: 0.5, Tokens: 10},
		{Role: "reviewer", Sessions: 1, Seconds: 10, CostUSD: 0.5, Tokens: 10},
	}
	if !slices.Equal(rep.TopRoles, wantRoles) {
		t.Errorf("top_roles =\n%+v\nwant\n%+v", rep.TopRoles, wantRoles)
	}
}

func TestBoardReportShouldFillDaysWithoutCost(t *testing.T) {
	f := newFixture(t)
	start := reportNow.AddDate(0, 0, -12)
	r := f.pastRun(t, f.repo, "#1", store.KindIssue, store.StateRecorded, "approved: ok", start, reportNow)
	f.spend(t, r.ID, "planner", start, 2, 10)
	// Before since, on its day: out of the period.
	f.spend(t, r.ID, "planner", reportNow.AddDate(0, 0, -7).Add(-4*time.Hour), 8, 10)
	f.spend(t, r.ID, "planner", reportNow.AddDate(0, 0, -5), 0.5, 10)
	f.spend(t, r.ID, "implementer", reportNow.AddDate(0, 0, -5).Add(time.Hour), 0.25, 10)
	f.spend(t, r.ID, "implementer", reportNow.AddDate(0, 0, -5).Add(2*time.Hour), 0.75, 10)
	f.spend(t, r.ID, "reviewer", reportNow.Add(-time.Hour), 0.25, 10)

	days := func(rep board.Report) map[string]map[string]float64 {
		out := map[string]map[string]float64{}
		for _, d := range rep.Daily {
			if d.ByRole == nil {
				t.Errorf("%s: by_role is {}, not null", d.Day)
			}
			out[d.Day] = d.ByRole
		}
		return out
	}
	rep := report(t, f, board.ReportQuery{Period: "7d"})
	var order []string
	for _, d := range rep.Daily {
		order = append(order, d.Day)
	}
	// From the day of since to today, both included.
	if want := []string{"2026-10-13", "2026-10-14", "2026-10-15", "2026-10-16", "2026-10-17", "2026-10-18", "2026-10-19", "2026-10-20"}; !slices.Equal(order, want) {
		t.Fatalf("days of 7d = %v, want %v", order, want)
	}
	got := days(rep)
	for day, want := range map[string]map[string]float64{
		"2026-10-13": {}, "2026-10-14": {}, "2026-10-15": {"planner": 0.5, "implementer": 1}, "2026-10-16": {},
		"2026-10-20": {"reviewer": 0.25},
	} {
		if !maps.Equal(got[day], want) {
			t.Errorf("%s = %v, want %v", day, got[day], want)
		}
	}

	all := report(t, f, board.ReportQuery{Period: "all"})
	// From the first day with cost.
	if len(all.Daily) != 13 || all.Daily[0].Day != "2026-10-08" || all.Daily[12].Day != "2026-10-20" {
		t.Errorf("days of all = %+v", all.Daily)
	}
	if got := days(all); !maps.Equal(got["2026-10-08"], map[string]float64{"planner": 2}) || !maps.Equal(got["2026-10-13"], map[string]float64{"planner": 8}) {
		t.Errorf("days of all = %v", got)
	}
}
