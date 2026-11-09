package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/control"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

const deadPID = 999_999

// fixture is a repository, a store and the control actions over them, as serve wires them.
type fixture struct {
	cfg  config.Config
	repo string
	st   *store.Store
	acts *control.Actions
	ops  *control.Operations
	// open are the issues the fake forge knows; others are closed.
	open map[int]string
	// exit is what the fake delamain binary exits with when launched.
	exit string
	sent []notify.Message
	mu   sync.Mutex
}

type issuesForge struct {
	forge.Forge
	f *fixture
}

func (g issuesForge) Issue(_ context.Context, n int) (forge.Issue, error) {
	if n == 500 {
		return forge.Issue{}, &forge.APIError{Status: 503}
	}
	if n == 404 {
		return forge.Issue{}, &forge.APIError{Status: 404}
	}
	title, ok := g.f.open[n]
	return forge.Issue{Number: n, Title: title, Open: ok}, nil
}

func (f *fixture) Notify(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fixture) Close() error { return nil }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{cfg: config.Default(), repo: filepath.Join(root, "app"), open: map[int]string{12: "Fix login", 21: "Add export"}, exit: "0"}
	f.cfg.Paths.StateDir = filepath.Join(root, "state")
	f.cfg.Paths.WorktreesDir = filepath.Join(root, "worktrees")
	f.cfg.Daemon.Repos = []string{f.repo}
	f.cfg.Review.Enabled = true
	f.cfg.Notify.Enabled = false
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", f.repo},
		{"-C", f.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(f.cfg.Paths.StateDir, "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	exe := filepath.Join(root, "delamain")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho launched \"$@\"\nexit $(cat '"+filepath.Join(root, "exit")+"')\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "exit"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.ops = control.NewOperations()
	f.acts = control.New(control.Deps{
		Cfg: f.cfg, Store: st, Operations: f.ops, RunIDPoll: 5 * time.Millisecond,
		Launcher: &control.Launcher{StateDir: f.cfg.Paths.StateDir, Executable: func() (string, error) { return exe, nil }},
		ForgeFor: func(context.Context, string) (forge.Forge, error) { return issuesForge{f: f}, nil },
		NewRunner: func() (*pipeline.Runner, error) {
			return &pipeline.Runner{Cfg: f.cfg, Store: st, Git: gitx.New(), Agents: pipeline.RPCAgents{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: f}, nil
		},
	})
	return f
}

// setExit makes the fake binary exit with code.
func (f *fixture) setExit(t *testing.T, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.repo), "exit"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run seeds a run owned by this process, with a worktree when branch is set.
func (f *fixture) run(t *testing.T, ref, branch string, state store.RunState, kind store.RunKind) store.Run {
	t.Helper()
	ctx := context.Background()
	rec, err := f.st.CreateRun(ctx, f.repo, ref, "Fix "+ref, "omp", 3, 0, kind)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration, rec.Branch, rec.Base = state, 1, branch, "main"
	if branch != "" {
		rec.Worktree = filepath.Join(f.cfg.Paths.WorktreesDir, "app", branch)
		if err := gitx.New().AddWorktree(ctx, f.repo, rec.Worktree, branch, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *fixture) orphan(t *testing.T, rec store.Run) {
	t.Helper()
	if ok, err := f.st.ClaimRun(context.Background(), rec.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal("seed a run whose process died", err)
	}
}

func (f *fixture) get(t *testing.T, id int64) store.Run {
	t.Helper()
	rec, err := f.st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// actionServer is a test server over the fixture's actions.
func actionServer(t *testing.T, f *fixture) *testServer {
	t.Helper()
	clk := newClock()
	auditPath := filepath.Join(f.cfg.Paths.StateDir, "api-audit.log")
	audit, err := OpenAudit(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	srv := New(&fakeSource{}, f.acts, audit, Options{Token: []byte(testToken), Now: clk.now, Interval: 20 * time.Millisecond, Version: "vtest"})
	ctx, cancel := context.WithCancel(context.Background())
	ts := httptest.NewServer(srv.Handler())
	go srv.Run(ctx)
	t.Cleanup(func() {
		cancel()
		ts.Close()
	})
	return &testServer{Server: ts, srv: srv, clock: clk, audit: auditPath}
}

func (ts *testServer) post(t *testing.T, path, body string) response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	return ts.do(t, http.MethodPost, path, testToken, r)
}

// waitOperation polls operation id until it leaves running.
func (ts *testServer) waitOperation(t *testing.T, id string) Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := ts.get(t, "/api/v1/operations/"+id)
		var op Operation
		res.json(t, &op)
		if op.Status != "running" {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation never ended: %+v", op)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopShouldRequestRecoverOrLeaveAlone(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	live := f.run(t, "#1", "", store.StateImplement, store.KindIssue)
	res := ts.post(t, "/api/v1/runs/1/stop", "")
	var out StopResult
	res.json(t, &out)
	if res.Status != http.StatusOK || out.Status != "requested" || out.RunID != live.ID || out.OwnerPID != os.Getpid() || out.State != "implement" {
		t.Fatalf("stop a live run: %d %+v", res.Status, out)
	}
	if stop, _ := f.st.StopRequested(context.Background(), live.ID); !stop {
		t.Fatal("the request is recorded")
	}

	done := f.run(t, "#2", "", store.StateRecorded, store.KindIssue)
	res = ts.post(t, "/api/v1/runs/2/stop", "")
	out = StopResult{}
	res.json(t, &out)
	if res.Status != http.StatusOK || out.Status != "already_finished" || out.RunID != done.ID {
		t.Fatalf("stop a finished run: %d %+v", res.Status, out)
	}

	orphan := f.run(t, "pr:7", "review/7", store.StateReview, store.KindReview)
	f.orphan(t, orphan)
	res = ts.post(t, "/api/v1/runs/3/stop", "")
	out = StopResult{}
	res.json(t, &out)
	if res.Status != http.StatusOK || out.Status != "recovered" || out.Recovered != "halted" {
		t.Fatalf("stop an orphan recovers it: %d %+v", res.Status, out)
	}
	if got := f.get(t, orphan.ID); got.State != store.StateRecorded {
		t.Fatalf("the orphan is recorded: %+v", got)
	}

	res = ts.post(t, "/api/v1/runs/99/stop", "")
	if p := res.problem(t); res.Status != http.StatusNotFound || p["code"] != codeNotFound {
		t.Fatalf("unknown run: %d %v", res.Status, p)
	}
	if res := ts.post(t, "/api/v1/runs/abc/stop", ""); res.Status != http.StatusBadRequest {
		t.Fatalf("bad id: %d", res.Status)
	}
	lines := ts.auditLines(t)
	if len(lines) != 5 || lines[0]["event"] != eventMutation || lines[0]["run_id"] != float64(1) || lines[0]["status"] != float64(200) || lines[3]["status"] != float64(404) {
		t.Fatalf("every stop is audited with its run: %v", lines)
	}
}

func TestRecoverShouldRunAsAnOperationAndRefuseWhatItCannot(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	orphan := f.run(t, "pr:7", "review/7", store.StateReview, store.KindReview)
	f.orphan(t, orphan)
	st := ts.openStream(t, "/api/v1/events")
	st.next(t, 4)

	res := ts.post(t, "/api/v1/runs/1/recover", "")
	var op Operation
	res.json(t, &op)
	if res.Status != http.StatusAccepted || op.Kind != "recover" || op.RunID != orphan.ID || op.ID == "" {
		t.Fatalf("recover: %d %+v", res.Status, op)
	}
	done := ts.waitOperation(t, op.ID)
	if done.Status != "succeeded" || done.Result != "halted" || done.EndedAt == nil {
		t.Fatalf("the operation ends with how the run ended: %+v", done)
	}
	var evs []sseEvent
	deadline := time.Now().Add(5 * time.Second)
	for len(evs) < 2 && time.Now().Before(deadline) {
		select {
		case ev := <-st.events:
			if ev.Name == "operation" {
				evs = append(evs, ev)
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	if len(evs) != 2 || !strings.Contains(evs[0].Data, `"status":"running"`) || !strings.Contains(evs[1].Data, `"status":"succeeded"`) {
		t.Fatalf("the stream carries the operation's start and end: %+v", evs)
	}

	res = ts.post(t, "/api/v1/runs/1/recover", "")
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeRunFinished || p["state"] != "recorded" {
		t.Fatalf("a finished run: %d %v", res.Status, p)
	}
	live := f.run(t, "#2", "", store.StateImplement, store.KindIssue)
	res = ts.post(t, "/api/v1/runs/2/recover", "")
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeOwnerAlive || p["owner_pid"] != float64(os.Getpid()) || p["run_id"] != float64(live.ID) {
		t.Fatalf("a live owner: %d %v", res.Status, p)
	}
	res = ts.get(t, "/api/v1/operations")
	var ops list[Operation]
	res.json(t, &ops)
	if len(ops.Items) != 1 || ops.Items[0].ID != op.ID {
		t.Fatalf("operations = %+v", ops)
	}
	if res := ts.get(t, "/api/v1/operations/nope"); res.Status != http.StatusNotFound {
		t.Fatalf("unknown operation: %d", res.Status)
	}
}

func TestDeleteRunShouldMapEveryRefusal(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	live := f.run(t, "#1", "fix/1", store.StateImplement, store.KindIssue)
	res := ts.do(t, http.MethodDelete, "/api/v1/runs/1", testToken, nil)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeRunNotFinished || p["state"] != "implement" {
		t.Fatalf("an unfinished run: %d %v", res.Status, p)
	}
	done := f.run(t, "#2", "fix/2", store.StateRecorded, store.KindIssue)
	if err := os.WriteFile(filepath.Join(done.Worktree, "wip.go"), []byte("package wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = ts.do(t, http.MethodDelete, "/api/v1/runs/2", testToken, nil)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeWorktreeDirty || p["worktree"] != done.Worktree {
		t.Fatalf("a dirty worktree: %d %v", res.Status, p)
	}
	res = ts.do(t, http.MethodDelete, "/api/v1/runs/2?force=true", testToken, nil)
	if res.Status != http.StatusNoContent {
		t.Fatalf("force: %d %s", res.Status, res.Body)
	}
	if _, err := os.Stat(done.Worktree); err == nil {
		t.Fatal("the worktree is gone")
	}
	shared := f.run(t, "#3", "fix/3", store.StateRecorded, store.KindIssue)
	live.Worktree = shared.Worktree
	if err := f.st.UpdateRun(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	res = ts.do(t, http.MethodDelete, "/api/v1/runs/3?force=true", testToken, nil)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeWorktreeInUse || p["run_id"] != float64(live.ID) {
		t.Fatalf("a worktree in use: %d %v", res.Status, p)
	}
	if res := ts.do(t, http.MethodDelete, "/api/v1/runs/99", testToken, nil); res.Status != http.StatusNotFound {
		t.Fatalf("unknown: %d", res.Status)
	}
	lines := ts.auditLines(t)
	if len(lines) != 5 || lines[2]["force"] != true || lines[2]["status"] != float64(204) || lines[1]["error"] != codeWorktreeDirty {
		t.Fatalf("deletes are audited with force and the outcome: %v", lines)
	}
}

func TestStartRunAndReviewShouldAnswerOperationsAndTheirRefusals(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	f.setExit(t, "5")
	res := ts.post(t, "/api/v1/runs", `{"repo":"`+f.repo+`","issue":12}`)
	var op Operation
	res.json(t, &op)
	if res.Status != http.StatusAccepted || op.Kind != "start_run" || op.Issue != 12 || op.Repo != f.repo || op.Status != "running" {
		t.Fatalf("start: %d %+v", res.Status, op)
	}
	done := ts.waitOperation(t, op.ID)
	if done.Status != "failed" || done.ExitCode == nil || *done.ExitCode != 5 || !strings.Contains(done.Error, "maximum parallelism") || !strings.Contains(done.LogTail, "launched run --issue 12") {
		t.Fatalf("exit 5: %+v", done)
	}

	for body, want := range map[string]struct {
		status int
		code   string
	}{
		`{"repo":"/elsewhere","issue":12}`:                {http.StatusForbidden, codeRepoNotAllowed},
		`{"repo":"` + f.repo + `","issue":0}`:             {http.StatusBadRequest, codeInvalidParam},
		`{"repo":"` + f.repo + `","issue":12,"kind":"x"}`: {http.StatusBadRequest, codeInvalidBody},
		`{"repo":"` + f.repo + `","issue":12,"zzz":1}`:    {http.StatusBadRequest, codeInvalidBody},
		`nope`: {http.StatusBadRequest, codeInvalidBody},
	} {
		res := ts.post(t, "/api/v1/runs", body)
		if p := res.problem(t); res.Status != want.status || p["code"] != want.code {
			t.Fatalf("%s: %d %v", body, res.Status, p)
		}
	}
	active := f.run(t, "#21", "", store.StateImplement, store.KindIssue)
	res = ts.post(t, "/api/v1/runs", `{"repo":"`+f.repo+`","issue":21}`)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeRunActive || p["run_id"] != float64(active.ID) {
		t.Fatalf("an active run: %d %v", res.Status, p)
	}

	f.setExit(t, "0")
	res = ts.post(t, "/api/v1/reviews", `{"repo":"`+f.repo+`","pr":7}`)
	res.json(t, &op)
	if res.Status != http.StatusAccepted || op.Kind != "start_review" || op.PR != 7 {
		t.Fatalf("review: %d %+v", res.Status, op)
	}
	if done := ts.waitOperation(t, op.ID); done.Status != "succeeded" || *done.ExitCode != 0 {
		t.Fatalf("exit 0: %+v", done)
	}
	f.cfg.Review.Enabled = false
	disabled := control.New(control.Deps{Cfg: f.cfg, Store: f.st})
	off := New(&fakeSource{}, disabled, nil, Options{Token: []byte(testToken)})
	offSrv := httptest.NewServer(off.Handler())
	t.Cleanup(offSrv.Close)
	offTS := &testServer{Server: offSrv, srv: off}
	res = offTS.post(t, "/api/v1/reviews", `{"repo":"`+f.repo+`","pr":7}`)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeReviewDisabled {
		t.Fatalf("reviews off: %d %v", res.Status, p)
	}
	lines := ts.auditLines(t)
	if lines[0]["repo"] != f.repo || lines[0]["issue"] != float64(12) || lines[0]["operation_id"] != op.ID && lines[0]["status"] != float64(202) {
		t.Fatalf("starts are audited with repo, issue and operation: %v", lines[0])
	}
}

func TestQueueActionsShouldAddRemoveAndMoveByID(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	res := ts.post(t, "/api/v1/queue", `{"repo":"`+f.repo+`","issue":12}`)
	var it QueueItem
	res.json(t, &it)
	if res.Status != http.StatusCreated || it.Issue != 12 || it.Title != "Fix login" || it.Position != 1 || it.ID == 0 || it.RepoName != "app" {
		t.Fatalf("add: %d %+v", res.Status, it)
	}
	res = ts.post(t, "/api/v1/queue", `{"repo":"`+f.repo+`","issue":12}`)
	var again QueueItem
	res.json(t, &again)
	if res.Status != http.StatusOK || again.ID != it.ID {
		t.Fatalf("add twice is 200 with the item: %d %+v", res.Status, again)
	}
	res = ts.post(t, "/api/v1/queue", `{"repo":"`+f.repo+`","issue":21}`)
	var second QueueItem
	res.json(t, &second)
	if res.Status != http.StatusCreated || second.Position != 2 {
		t.Fatalf("second: %d %+v", res.Status, second)
	}
	for body, want := range map[string]struct {
		status int
		code   string
	}{
		`{"repo":"` + f.repo + `","issue":40}`:  {http.StatusConflict, codeIssueClosed},
		`{"repo":"` + f.repo + `","issue":404}`: {http.StatusNotFound, codeIssueNotFound},
		`{"repo":"` + f.repo + `","issue":500}`: {http.StatusBadGateway, codeForgeError},
		`{"repo":"/elsewhere","issue":12}`:      {http.StatusForbidden, codeRepoNotAllowed},
	} {
		res := ts.post(t, "/api/v1/queue", body)
		if p := res.problem(t); res.Status != want.status || p["code"] != want.code {
			t.Fatalf("%s: %d %v", body, res.Status, p)
		}
	}
	active := f.run(t, "#5", "", store.StateImplement, store.KindIssue)
	f.open[5] = "running one"
	res = ts.post(t, "/api/v1/queue", `{"repo":"`+f.repo+`","issue":5}`)
	if p := res.problem(t); res.Status != http.StatusConflict || p["code"] != codeRunActive || p["run_id"] != float64(active.ID) {
		t.Fatalf("an active run: %d %v", res.Status, p)
	}

	res = ts.do(t, http.MethodPut, "/api/v1/queue/"+itoa(second.ID)+"/position", testToken, strings.NewReader(`{"position":1}`))
	var moved list[QueueItem]
	res.json(t, &moved)
	if res.Status != http.StatusOK || len(moved.Items) != 2 || moved.Items[0].ID != second.ID || moved.Items[1].Position != 2 {
		t.Fatalf("move: %d %+v", res.Status, moved)
	}
	res = ts.do(t, http.MethodPut, "/api/v1/queue/"+itoa(second.ID)+"/position", testToken, strings.NewReader(`{"position":0}`))
	if p := res.problem(t); res.Status != http.StatusBadRequest || p["code"] != codeInvalidBody {
		t.Fatalf("position 0: %d %v", res.Status, p)
	}
	res = ts.do(t, http.MethodPut, "/api/v1/queue/999/position", testToken, strings.NewReader(`{"position":1}`))
	if p := res.problem(t); res.Status != http.StatusNotFound || p["code"] != codeNotFound {
		t.Fatalf("move unknown: %d %v", res.Status, p)
	}
	if res := ts.do(t, http.MethodDelete, "/api/v1/queue/"+itoa(it.ID), testToken, nil); res.Status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", res.Status, res.Body)
	}
	if res := ts.do(t, http.MethodDelete, "/api/v1/queue/"+itoa(it.ID), testToken, nil); res.Status != http.StatusNotFound {
		t.Fatalf("delete twice: %d", res.Status)
	}
	if res := ts.do(t, http.MethodDelete, "/api/v1/queue/x", testToken, nil); res.Status != http.StatusBadRequest {
		t.Fatalf("bad id: %d", res.Status)
	}
	items, err := f.st.Queue(context.Background())
	if err != nil || len(items) != 1 || items[0].Issue != 21 {
		t.Fatalf("queue = %+v %v", items, err)
	}
	lines := ts.auditLines(t)
	if lines[0]["queue_id"] != float64(it.ID) || lines[0]["issue"] != float64(12) || lines[0]["status"] != float64(201) {
		t.Fatalf("queue adds are audited with the item: %v", lines[0])
	}
}

func TestActionRoutesAreAbsentOnAReadOnlyServer(t *testing.T) {
	ts := newTestServer(t, &fakeSource{})
	if res := ts.post(t, "/api/v1/runs/1/stop", ""); res.Status != http.StatusNotFound {
		t.Fatalf("no actions without control: %d", res.Status)
	}
	if res := ts.get(t, "/api/v1/operations"); res.Status != http.StatusNotFound {
		t.Fatalf("no operations without control: %d", res.Status)
	}
}

func TestGETOnAnActionPathIs405(t *testing.T) {
	f := newFixture(t)
	ts := actionServer(t, f)
	res := ts.get(t, "/api/v1/runs/1/stop")
	if res.Status != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "POST" {
		t.Fatalf("GET on stop: %d allow=%q", res.Status, res.Header.Get("Allow"))
	}
	res = ts.get(t, "/api/v1/reviews")
	if res.Status != http.StatusMethodNotAllowed {
		t.Fatalf("GET on reviews: %d", res.Status)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMapErrShouldNameThePendingOperationWhenTheIssueHasNoRunYet(t *testing.T) {
	pending := mapErr(&control.RunError{Ref: "#7", Operation: "op-1-1", Err: control.ErrRunActive})
	if pending.status != http.StatusConflict || pending.code != codeRunActive || pending.extra["operation_id"] != "op-1-1" {
		t.Fatalf("a start still launching is run_active with its operation: %+v", pending)
	}
	if _, ok := pending.extra["run_id"]; ok {
		t.Fatalf("there is no run to name yet: %+v", pending.extra)
	}
	active := mapErr(&control.RunError{ID: 3, State: "implement", Ref: "#7", Err: control.ErrRunActive})
	if active.extra["run_id"] != int64(3) || active.extra["state"] != "implement" {
		t.Fatalf("an active run is still named by its id: %+v", active.extra)
	}
}
