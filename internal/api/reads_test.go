package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
)

var t0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func sampleRuns(dir string) []board.Run {
	return []board.Run{
		{ID: 1, Kind: "issue", Repo: "/home/me/projects/app", Issue: "#12", Title: "Fix login", State: "recorded", Reason: "approved: merged",
			Iteration: 2, MaxIterations: 3, Branch: "feat/12-login", PR: "https://github.com/me/app/pull/31", OwnerPID: 4242,
			Usage: board.Spend{Mode: "tokens", Value: 167000, Limit: 2000000, CostUSD: 0.67}, Dir: dir, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour)},
		{ID: 2, Kind: "issue", Repo: "/home/me/projects/app", Issue: "#13", Title: "Add export", State: "implement", Iteration: 1, MaxIterations: 3,
			OwnerPID: 4243, Dir: dir, CreatedAt: t0.Add(time.Hour), UpdatedAt: t0.Add(2 * time.Hour)},
		{ID: 3, Kind: "review", Repo: "/home/me/projects/api", Issue: "pr:7", Title: "Review PR 7", State: "review", Orphaned: true, OwnerPID: 999999,
			Dir: dir, CreatedAt: t0.Add(2 * time.Hour), UpdatedAt: t0.Add(3 * time.Hour)},
		{ID: 4, Kind: "issue", Repo: "/home/me/projects/app", Issue: "#14", Title: "Needs info", State: "needs_info", Reason: "needs_info: sem critérios",
			OwnerPID: 4244, Dir: dir, CreatedAt: t0.Add(3 * time.Hour), UpdatedAt: t0.Add(4 * time.Hour)},
	}
}

func TestStatusShouldCarryNumbersTheVersionAndTheServerTime(t *testing.T) {
	src := &fakeSource{status: board.Status{Daemon: true, SlotsInUse: 1, SlotsMax: 3, QueueRunning: true, Today: "8.1M", TodayLimit: "10M", TodayLevel: "soft",
		TodaySpend: board.Spend{Mode: "tokens", Value: 8_100_000, Limit: 10_000_000, CostUSD: 32.4}}}
	ts := newTestServer(t, src)
	res := ts.get(t, "/api/v1/status")
	var st Status
	res.json(t, &st)
	if res.Status != http.StatusOK || !st.Daemon || st.SlotsInUse != 1 || st.SlotsMax != 3 || !st.QueueRunning || st.TodayLevel != "soft" {
		t.Fatalf("status = %+v", st)
	}
	if st.Today != (board.Spend{Mode: "tokens", Value: 8_100_000, Limit: 10_000_000, CostUSD: 32.4}) {
		t.Fatalf("the spend is numeric: %+v", st.Today)
	}
	if st.Version != "vtest" || !st.ServerTime.Equal(ts.clock.now()) {
		t.Fatalf("version and server time: %+v", st)
	}
	if !strings.Contains(res.Body, `"today":{"mode":"tokens","value":8100000,"limit":10000000,"cost_usd":32.4}`) {
		t.Fatalf("json = %s", res.Body)
	}
}

func TestReposShouldListTheAllowedRepositories(t *testing.T) {
	src := &fakeSource{repos: []Repo{{Path: "/home/me/projects/app", Name: "app", Forge: &ForgeRef{Kind: "github", Host: "github.com", Path: "me/app"}}}}
	ts := newTestServer(t, src)
	res := ts.get(t, "/api/v1/repos")
	var got list[Repo]
	res.json(t, &got)
	if res.Status != http.StatusOK || len(got.Items) != 1 || got.Items[0].Forge.Path != "me/app" {
		t.Fatalf("repos = %+v", got)
	}
	if res := newTestServer(t, &fakeSource{}).get(t, "/api/v1/repos"); strings.TrimSpace(res.Body) != `{"items":[]}` {
		t.Fatalf("no repos is an empty list: %s", res.Body)
	}
}

func TestRunsShouldPageNewestFirstWithFullRepoPathsAndNumericSpend(t *testing.T) {
	src := &fakeSource{runs: sampleRuns(t.TempDir())}
	ts := newTestServer(t, src)
	res := ts.get(t, "/api/v1/runs?limit=2")
	var page list[RunSummary]
	res.json(t, &page)
	if res.Status != http.StatusOK || len(page.Items) != 2 || page.Items[0].ID != 4 || page.Items[1].ID != 3 || page.NextBefore == nil || *page.NextBefore != 3 {
		t.Fatalf("first page = %+v", page)
	}
	orphan := page.Items[1]
	if orphan.Repo != "/home/me/projects/api" || orphan.RepoName != "api" || orphan.Ref != "pr:7" || orphan.Kind != "review" {
		t.Fatalf("the full path, its base name and the ref: %+v", orphan)
	}
	if !orphan.Orphaned || orphan.Live || orphan.Outcome != "orphaned" || orphan.Column != board.ColHuman || orphan.Owner != (Owner{PID: 999999, Alive: false}) {
		t.Fatalf("an orphan: %+v", orphan)
	}
	needsInfo := page.Items[0]
	if needsInfo.Live || needsInfo.Outcome != "needs_info" || needsInfo.Reason != "sem critérios" || needsInfo.Column != board.ColHuman {
		t.Fatalf("needs_info: %+v", needsInfo)
	}

	res = ts.get(t, "/api/v1/runs?limit=2&before=3")
	page = list[RunSummary]{}
	res.json(t, &page)
	if len(page.Items) != 2 || page.Items[0].ID != 2 || page.Items[1].ID != 1 || page.NextBefore != nil {
		t.Fatalf("last page = %+v", page)
	}
	done := page.Items[1]
	if done.Live || done.Outcome != "approved" || done.Reason != "merged" || done.Column != board.ColDone || done.Owner.Alive {
		t.Fatalf("a finished run: %+v", done)
	}
	if done.Spend != (board.Spend{Mode: "tokens", Value: 167000, Limit: 2000000, CostUSD: 0.67}) || done.PR == "" || done.Branch == "" {
		t.Fatalf("numeric spend and the links: %+v", done)
	}
	live := page.Items[0]
	if !live.Live || live.Column != board.ColImplement || live.Owner != (Owner{PID: 4243, Alive: true}) || live.Outcome != "" {
		t.Fatalf("a live run: %+v", live)
	}
	if !strings.Contains(res.Body, `"rounds":[]`) {
		t.Fatalf("rounds are a list even when empty: %s", res.Body)
	}

	res = ts.get(t, "/api/v1/runs?state=active")
	page = list[RunSummary]{}
	res.json(t, &page)
	if len(page.Items) != 2 || page.Items[0].ID != 3 || page.Items[1].ID != 2 {
		t.Fatalf("active = %+v", page)
	}
	res = ts.get(t, "/api/v1/runs?state=finished")
	page = list[RunSummary]{}
	res.json(t, &page)
	if len(page.Items) != 2 || page.Items[0].ID != 4 || page.Items[1].ID != 1 {
		t.Fatalf("finished = %+v", page)
	}
	for _, q := range []string{"state=weird", "limit=0", "limit=201", "limit=x", "before=-1", "before=0", "before=abc"} {
		res := ts.get(t, "/api/v1/runs?"+q)
		if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidParam {
			t.Fatalf("%s: %d %v", q, res.Status, p)
		}
	}
}

func TestRunShouldCarryTheArtifactsOrSayItIsNotFound(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "iter-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), []byte(`{"slices":[{"title":"s1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Report\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := sampleRuns(dir)
	runs[0].Agents = []board.Agent{{Role: "planner", State: "done", StartedAt: t0, Tokens: 1000, CostUSD: 0.01}}
	ts := newTestServer(t, &fakeSource{runs: runs})
	res := ts.get(t, "/api/v1/runs/1")
	var d RunDetail
	res.json(t, &d)
	if res.Status != http.StatusOK || d.ID != 1 || d.RunDir != dir || d.Report != "# Report\n" || d.Plan == nil || len(d.Iterations) != 2 || len(d.Agents) != 1 {
		t.Fatalf("detail = %+v", d)
	}
	if d.Outcome != "approved" || d.Repo != "/home/me/projects/app" {
		t.Fatalf("the summary is embedded: %+v", d.RunSummary)
	}
	res = ts.get(t, "/api/v1/runs/42")
	if p := res.problem(t); res.Status != http.StatusNotFound || p["code"] != codeNotFound {
		t.Fatalf("unknown run: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/runs/abc")
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidParam {
		t.Fatalf("bad id: %d %v", res.Status, p)
	}
}

func TestQueueShouldExposeTheItemIDsAndTheRepoNames(t *testing.T) {
	src := &fakeSource{queue: []board.QueueItem{
		{ID: 11, Position: 1, Repo: "/home/me/projects/app", Issue: 12, Title: "Fix login", AddedAt: t0},
		{ID: 9, Position: 2, Repo: "/home/me/projects/api", Issue: 3, Title: "Add export", AddedAt: t0, Waiting: "depends on #2"},
	}}
	ts := newTestServer(t, src)
	res := ts.get(t, "/api/v1/queue")
	var got list[QueueItem]
	res.json(t, &got)
	if res.Status != http.StatusOK || len(got.Items) != 2 || got.Items[0].ID != 11 || got.Items[1].ID != 9 || got.Items[1].RepoName != "api" || got.Items[1].Waiting != "depends on #2" {
		t.Fatalf("queue = %+v", got)
	}
	if res := newTestServer(t, &fakeSource{}).get(t, "/api/v1/queue"); strings.TrimSpace(res.Body) != `{"items":[]}` {
		t.Fatalf("an empty queue is an empty list: %s", res.Body)
	}
}

func TestReportAndBoardShouldPassTheBoardShapesThrough(t *testing.T) {
	ts := newTestServer(t, &fakeSource{runs: sampleRuns(t.TempDir())})
	res := ts.get(t, "/api/v1/report?period=7d")
	if res.Status != http.StatusOK || !strings.Contains(res.Body, `"period":"7d"`) {
		t.Fatalf("report: %d %s", res.Status, res.Body)
	}
	if res := ts.get(t, "/api/v1/report?period=yesterday"); res.Status != http.StatusBadRequest {
		t.Fatalf("a bad period: %d", res.Status)
	}
	res = ts.get(t, "/api/v1/board")
	var snap board.Snapshot
	res.json(t, &snap)
	if res.Status != http.StatusOK || len(snap.Columns) != 9 {
		t.Fatalf("board: %d %+v", res.Status, snap)
	}
}

func TestReadsShouldAnswer500WhenTheSourceFails(t *testing.T) {
	ts := newTestServer(t, &fakeSource{err: os.ErrPermission})
	for _, path := range []string{"/api/v1/status", "/api/v1/runs", "/api/v1/queue", "/api/v1/board"} {
		res := ts.get(t, path)
		if p := res.problem(t); res.Status != http.StatusInternalServerError || p["code"] != codeInternal {
			t.Fatalf("%s: %d %v", path, res.Status, p)
		}
	}
}
