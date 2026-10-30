package cli

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/store"
)

func TestBoardShouldRejectBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"board", "--addr", "0.0.0.0:7777"},
		{"board", "--addr", ":7777"},
		{"board", "--addr", "192.168.0.10:7777"},
		{"board", "--addr", "localhost"},
		{"board", "--interval", "0s"},
		{"board", "--limit", "-1"},
	} {
		if out, err := execute(t, args...); err == nil {
			t.Errorf("%v: no error\n%s", args, out)
		}
	}
	if err := (boardOptions{addr: "0.0.0.0:7777", interval: time.Second, allowRemote: true}).validate(); err != nil {
		t.Fatalf("--allow-remote must accept any address: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:7777", "[::1]:7777"} {
		if err := (boardOptions{addr: addr, interval: time.Second}).validate(); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestBoardShouldServeTheRunsOfTheStore(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	planning := f.run(t, "#1", "", store.StatePlan)
	orphan := f.run(t, "#2", "", store.StateImplement)
	if ok, err := f.st.ClaimRun(ctx, orphan.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal(err)
	}
	done := f.run(t, "#3", "", store.StateRecorded)
	done.Reason = "approved: PR aberto"
	if err := f.st.UpdateRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Enqueue(ctx, f.repo, 4, "Next"); err != nil {
		t.Fatal(err)
	}

	in, closeIn, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeIn()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serveBoard(ctx, ln, boardSource{in: in}, boardOptions{interval: 50 * time.Millisecond, limit: 20})
	}()

	res, err := http.Get("http://" + ln.Addr().String() + "/api/snapshot") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var snap board.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	got := map[string]string{}
	for _, col := range snap.Columns {
		for _, c := range col.Cards {
			got[c.Key] = col.Key
		}
	}
	want := map[string]string{
		"run-" + strconv.FormatInt(planning.ID, 10): board.ColPlan,
		"run-" + strconv.FormatInt(orphan.ID, 10):   board.ColHuman,
		"run-" + strconv.FormatInt(done.ID, 10):     board.ColDone,
		"queue-1":                                   board.ColQueue,
	}
	for k, col := range want {
		if got[k] != col {
			t.Errorf("%s in %q, want %q (board: %v)", k, got[k], col, got)
		}
	}
	if !strings.Contains(string(body), `"today_level":"ok"`) {
		t.Errorf("no day's spend in %s", body)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveBoard: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoard did not stop with its context")
	}
}

func TestBoardQueueShouldExposeWhatAnItemWaitsFor(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked, err := f.st.Enqueue(ctx, f.repo, 4, "Blocked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Enqueue(ctx, f.repo, 5, "Free"); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetQueueWaiting(ctx, blocked.ID, "blocked by #7"); err != nil {
		t.Fatal(err)
	}

	in, closeIn, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeIn()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serveBoard(ctx, ln, boardSource{in: in}, boardOptions{interval: 50 * time.Millisecond, limit: 20})
	}()

	res, err := http.Get("http://" + ln.Addr().String() + "/api/snapshot") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var snap struct {
		Columns []struct {
			Key   string           `json:"key"`
			Cards []map[string]any `json:"cards"`
		} `json:"columns"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	cards := map[string]map[string]any{}
	for _, col := range snap.Columns {
		if col.Key != board.ColQueue {
			continue
		}
		for _, c := range col.Cards {
			key, _ := c["key"].(string)
			cards[key] = c
		}
	}
	if got := cards["queue-1"]["waiting"]; got != "blocked by #7" {
		t.Errorf("waiting of the blocked item = %v, want %q (body: %s)", got, "blocked by #7", body)
	}
	if c, ok := cards["queue-2"]; !ok {
		t.Errorf("no queue-2 in %s", body)
	} else if got, ok := c["waiting"]; ok {
		t.Errorf("an item that waits for nothing has waiting = %v (body: %s)", got, body)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveBoard: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoard did not stop with its context")
	}
}

func TestBoardShouldServeTheReportOfTheStore(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := f.run(t, "#1", "", store.StateRecorded)
	done.Reason, done.Iteration = "approved: PR aberto", 2
	if err := f.st.UpdateRun(ctx, done); err != nil {
		t.Fatal(err)
	}
	active := f.run(t, "#2", "", store.StateImplement)
	// Runs from before agent_sessions only have agents rows and the ledger.
	for _, a := range []struct {
		run   int64
		role  string
		usage store.Usage
	}{
		{done.ID, "planner", store.Usage{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 10, CostUSD: 0.4}},
		{done.ID, "implementer", store.Usage{Input: 300, Output: 200, CacheWrite: 5, CostUSD: 1.1}},
		{active.ID, "implementer", store.Usage{Input: 40, Output: 2, CostUSD: 0.25}},
	} {
		if err := f.st.StartAgent(ctx, store.Agent{RunID: a.run, Role: a.role}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.RecordUsage(ctx, a.run, a.role, "/s/"+a.role+strconv.FormatInt(a.run, 10), a.usage); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"planner", "implementer"} {
		if err := f.st.EndAgent(ctx, done.ID, role, store.AgentDone); err != nil {
			t.Fatal(err)
		}
	}

	in, closeIn, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeIn()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- serveBoard(ctx, ln, boardSource{in: in}, boardOptions{interval: 50 * time.Millisecond, limit: 20})
	}()

	res, err := http.Get("http://" + ln.Addr().String() + "/api/report?period=30d") //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	var rep board.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(rep.History) != 2 || rep.History[0].ID != active.ID || rep.History[1].ID != done.ID {
		t.Fatalf("history is the runs of the period, newest first: %s", body)
	}
	got := rep.History[1]
	if got.Ref != "#1" || got.Title != "Fix #1" || got.Repo != f.repo || got.Kind != "issue" || got.State != "recorded" ||
		got.Outcome != "approved" || got.Iterations != 2 || got.EndedAt == nil || got.StartedAt.IsZero() {
		t.Errorf("finished run = %+v", got)
	}
	if a := rep.History[0]; a.Outcome != "" || a.EndedAt != nil || a.State != "implement" || a.Seconds < 0 {
		t.Errorf("a running run has no outcome nor end: %+v", a)
	}

	// The cost and tokens of a run are what `delamain ls` shows for its agents.
	var runs []RunView
	if err := json.Unmarshal([]byte(f.exec(t, "ls", "--all", "-o", "json")), &runs); err != nil {
		t.Fatal(err)
	}
	listed := map[int64]RunView{}
	for _, v := range runs {
		listed[v.ID] = v
	}
	for _, r := range rep.History {
		var cost float64
		var tokens int64
		for _, a := range listed[r.ID].Agents {
			cost += a.CostUSD
			tokens += a.Tokens
		}
		if r.CostUSD != cost || r.Tokens != tokens {
			t.Errorf("run %d in the report: US$ %v and %d tokens; in ls: US$ %v and %d tokens", r.ID, r.CostUSD, r.Tokens, cost, tokens)
		}
	}
	if rep.History[1].CostUSD != 1.5 || rep.History[1].Tokens != 665 {
		t.Errorf("finished run = US$ %v and %d tokens, want US$ 1.5 and 665", rep.History[1].CostUSD, rep.History[1].Tokens)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveBoard: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveBoard did not stop with its context")
	}
}

func TestBoardSourceReportShouldAnswerEmptyListsNotNull(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in, closeIn, err := openInspector(ctx, func() (config.Config, error) { return config.Load(f.cfgFile, os.Getenv) })
	if err != nil {
		t.Fatal(err)
	}
	defer closeIn()
	r, err := boardSource{in: in}.Report(ctx, board.ReportQuery{Period: "all"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"period":"all"`, `"repos":[]`, `"daily":[]`, `"top_runs":[]`, `"top_roles":[]`, `"top_sessions":[]`,
		`"history":[]`, `"next_offset":null`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("no %s in %s", want, body)
		}
	}
}
