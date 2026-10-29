package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/artifacts"
)

type fakeSource struct {
	mu     sync.Mutex
	runs   []Run
	queue  []QueueItem
	status Status
	report Report
	query  ReportQuery // the last one Report received
	err    error
}

func (f *fakeSource) Runs(context.Context, int) ([]Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Run(nil), f.runs...), f.err
}

func (f *fakeSource) Run(_ context.Context, id int64) (Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return Run{}, fmt.Errorf("run %d: %w", id, ErrNotFound)
}

func (f *fakeSource) Queue(context.Context) ([]QueueItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]QueueItem(nil), f.queue...), nil
}

func (f *fakeSource) Status(context.Context) (Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}

func (f *fakeSource) Report(_ context.Context, q ReportQuery) (Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.query = q
	return f.report, f.err
}

func (f *fakeSource) set(fn func(*fakeSource)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func write(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, ok := v.([]byte)
	if !ok {
		var err error
		if data, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func review(blocking map[string]int) artifacts.Review {
	var r artifacts.Review
	for _, axis := range artifacts.RequiredAxes {
		a := artifacts.Axis{Axis: axis, Executed: true, Findings: []artifacts.Finding{{Severity: "minor", Message: "nit"}}}
		for range blocking[axis] {
			a.Findings = append(a.Findings, artifacts.Finding{Severity: "major", File: "a.go", Line: 3, Message: "bug"})
		}
		r.Axes = append(r.Axes, a)
	}
	return r
}

func chipsOf(r Round) string {
	var out []string
	for _, c := range r.Chips {
		out = append(out, c.Label+"="+c.Tone)
	}
	return r.Label + " " + strings.Join(out, " ")
}

func TestRoundsOfAnIssueRun(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "logs", "delamain.log"), []byte(strings.Join([]string{
		"10:00:00 state run=1 state=implement iteration=1",
		"10:00:01 state run=1 state=verify iteration=1",
		"10:00:02 gate run=1 name=test exit=0",
		"10:00:03 state run=1 state=review iteration=1",
		"10:01:00 state run=1 state=implement iteration=2",
		"10:01:01 state run=1 state=verify iteration=2",
		"10:01:02 gate run=1 name=test exit=2",
		"10:02:00 state run=1 state=implement iteration=3",
		"10:02:01 state run=1 state=verify iteration=3",
		"10:02:02 gate run=1 name=test exit=0",
		"10:02:03 state run=1 state=review iteration=3",
		"10:03:00 state run=1 state=implement iteration=4",
		"10:03:01 state run=1 state=verify iteration=4",
		"10:03:02 gate run=1 name=test exit=0",
		"10:03:03 state run=1 state=review iteration=4",
	}, "\n")+"\n"))
	write(t, filepath.Join(dir, "iter-1", "review.json"), review(map[string]int{"security": 2}))
	write(t, filepath.Join(dir, "iter-3", "review.json"), []byte(`{"axes": [`))
	b := &builder{files: newFiles()}

	r := Run{ID: 1, State: "review", Iteration: 4, Dir: dir}
	got := b.rounds(r)
	want := []string{
		"R1 gates=pass spec=pass std=pass sec 2=changes",
		"R2 gates ✗=fail",
		"R3 gates=pass spec?=bad std?=bad sec?=bad",
		"R4 gates=pass spec=wait std=wait sec=wait",
	}
	if len(got) != len(want) {
		t.Fatalf("rounds = %+v", got)
	}
	for i := range want {
		if chipsOf(got[i]) != want[i] {
			t.Errorf("round %d = %q, want %q", i+1, chipsOf(got[i]), want[i])
		}
	}
	if got[0].Verdict != artifacts.VerdictChangesRequested {
		t.Errorf("verdict of R1 = %q", got[0].Verdict)
	}

	// The review of the last iteration landed: its chips fill in.
	write(t, filepath.Join(dir, "iter-4", "review.json"), review(nil))
	got = b.rounds(r)
	if s := chipsOf(got[3]); s != "R4 gates=pass spec=pass std=pass sec=pass" || got[3].Verdict != artifacts.VerdictApprove {
		t.Fatalf("R4 after the review = %q (%s)", s, got[3].Verdict)
	}

	// A run halted in the middle of a review has no review.json: it is missing, not pending.
	r.State, r.Reason = "recorded", "halted: parada"
	if err := os.Remove(filepath.Join(dir, "iter-4", "review.json")); err != nil {
		t.Fatal(err)
	}
	if s := chipsOf(b.rounds(r)[3]); s != "R4 gates=pass spec?=bad std?=bad sec?=bad" {
		t.Fatalf("R4 of a halted run = %q", s)
	}
}

func TestAxisNotExecuted(t *testing.T) {
	rv := review(nil)
	rv.Axes[1].Executed = false
	rv.Axes = rv.Axes[:2] // security missing
	got := chipsOf(Round{Chips: axisChips(rv.Axes)})
	if got != " spec=pass std?=bad sec?=bad" {
		t.Fatalf("chips = %q", got)
	}
}

func TestRoundsOfPRRuns(t *testing.T) {
	dir := t.TempDir()
	b := &builder{files: newFiles()}
	pr := Run{ID: 2, Kind: "review", State: "review", Dir: dir}
	if s := chipsOf(b.rounds(pr)[0]); s != "PR spec=wait std=wait sec=wait" {
		t.Fatalf("PR review in progress = %q", s)
	}
	write(t, filepath.Join(dir, "review.json"), artifacts.PRReview{Verdict: "concerns", Adherence: "partial", Summary: "s", Axes: review(map[string]int{"spec": 1}).Axes})
	got := b.rounds(pr)[0]
	if s := chipsOf(got); s != "PR spec 1=changes std=pass sec=pass" || got.Verdict != "partial" {
		t.Fatalf("PR review = %q (%s)", s, got.Verdict)
	}

	addr := Run{ID: 3, Kind: "address", State: "recorded", Dir: t.TempDir()}
	if r := b.rounds(addr); r != nil {
		t.Fatalf("address without responses = %+v", r)
	}
	write(t, filepath.Join(addr.Dir, "responses.json"), artifacts.Responses{Responses: []artifacts.Response{
		{ThreadID: "a", Verdict: "valid"}, {ThreadID: "b", Verdict: "valid"}, {ThreadID: "c", Verdict: "invalid"},
	}})
	if s := chipsOf(b.rounds(addr)[0]); s != "PR valid 2=pass invalid 1=idle" {
		t.Fatalf("address = %q", s)
	}
}

func TestSnapshotPlacesEveryCard(t *testing.T) {
	src := &fakeSource{
		queue: []QueueItem{
			{Position: 1, Repo: "/src/app", Issue: 9, Title: "next", Waiting: "blocked by #7"},
			{Position: 2, Repo: "/src/app", Issue: 10, Title: "then"},
		},
		runs: []Run{
			{ID: 1, Repo: "/src/app", State: "plan", Dir: t.TempDir()},
			{ID: 2, Repo: "/src/app", State: "implement", Orphaned: true, Dir: t.TempDir()},
			{ID: 3, Repo: "/src/app", State: "recorded", Reason: "approved: PR aberto", Dir: t.TempDir()},
			{ID: 4, Repo: "/src/app", State: "recorded", Reason: "exhausted: 3 iterações", Dir: t.TempDir()},
		},
		status: Status{Daemon: true, SlotsMax: 2},
	}
	b := &builder{src: src, files: newFiles(), limit: 20}
	snap, err := b.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	where := map[string]Card{}
	for _, col := range snap.Columns {
		for _, c := range col.Cards {
			where[col.Key+"/"+c.Key] = c
		}
	}
	for _, key := range []string{"queue/queue-1", "queue/queue-2", "plan/run-1", "human/run-2", "done/run-3", "human/run-4"} {
		if _, ok := where[key]; !ok {
			t.Errorf("no card %s in %v", key, where)
		}
	}
	if c := where["queue/queue-1"]; c.Kind != "queued" || c.Waiting != "blocked by #7" {
		t.Errorf("waiting queued card = %+v", c)
	}
	if c := where["queue/queue-2"]; c.Waiting != "" {
		t.Errorf("queued card that waits for nothing = %+v", c)
	}
	if c := where["plan/run-1"]; !c.Live || c.Repo != "app" || c.Kind != "issue" {
		t.Errorf("active card = %+v", c)
	}
	if c := where["human/run-2"]; c.Live || c.Hint != "delamain recover" {
		t.Errorf("orphaned card = %+v", c)
	}
	if c := where["done/run-3"]; c.Outcome != "approved" || c.Reason != "PR aberto" || c.Hint != "" {
		t.Errorf("approved card = %+v", c)
	}
	if c := where["human/run-4"]; c.Outcome != "exhausted" || c.Hint != "delamain logs 4" {
		t.Errorf("exhausted card = %+v", c)
	}
	if len(snap.Columns) != 9 || snap.Columns[0].Key != ColQueue || snap.Columns[8].Key != ColHuman {
		t.Errorf("columns = %+v", snap.Columns)
	}
}

func TestDetail(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "plan.json"), artifacts.Plan{Summary: "do it", Kind: "feature", Risks: []string{"r"}})
	write(t, filepath.Join(dir, "iter-1", "impl.json"), artifacts.Impl{Slices: []artifacts.SliceResult{{ID: "s1", Status: "done"}}})
	write(t, filepath.Join(dir, "iter-1", "verify-test.log"), []byte("ok\nFAIL x\n"))
	write(t, filepath.Join(dir, "iter-2", "review.json"), review(nil))
	write(t, filepath.Join(dir, "report.md"), []byte("# relatório"))
	write(t, filepath.Join(dir, "logs", "delamain.log"), []byte(
		"10:00:00 state run=5 state=verify iteration=1\n10:00:01 gate run=5 name=test exit=1\n10:00:02 gate run=5 name=../../etc exit=1\n"))
	started := time.Now()
	src := &fakeSource{runs: []Run{{ID: 5, State: "recorded", Reason: "approved", Iteration: 2, Dir: dir,
		Agents: []Agent{{Role: "planner", State: "done", StartedAt: started}}}}}
	b := &builder{src: src, files: newFiles()}

	d, err := b.detail(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if d.Plan == nil || d.Plan.Summary != "do it" || d.Report != "# relatório" || len(d.Agents) != 1 || d.Column != ColDone {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Iterations) != 2 {
		t.Fatalf("iterations = %+v", d.Iterations)
	}
	it := d.Iterations[0]
	if it.Impl == nil || len(it.Gates) != 2 || it.Gates[0].Tail != "ok\nFAIL x" || it.Gates[1].Tail != "" {
		t.Fatalf("iteration 1 = %+v", it)
	}
	if d.Iterations[1].Verdict != artifacts.VerdictApprove {
		t.Fatalf("iteration 2 = %+v", d.Iterations[1])
	}
	if _, err := b.detail(context.Background(), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detail of a missing run: %v", err)
	}
}

func TestFilesRereadOnlyWhatChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	f := newFiles()
	if _, ok, err := readJSON[artifacts.Plan](f, path); ok || err != nil {
		t.Fatalf("missing file: ok %v err %v", ok, err)
	}
	write(t, path, artifacts.Plan{Summary: "one"})
	p, ok, err := readJSON[artifacts.Plan](f, path)
	if !ok || err != nil || p.Summary != "one" {
		t.Fatalf("first read: %+v %v %v", p, ok, err)
	}
	write(t, path, artifacts.Plan{Summary: "second"})
	if p, _, _ := readJSON[artifacts.Plan](f, path); p.Summary != "second" {
		t.Fatalf("changed file not reread: %+v", p)
	}
	write(t, path, []byte(`{"summary": `))
	if _, ok, err := readJSON[artifacts.Plan](f, path); !ok || err == nil {
		t.Fatalf("partial file: ok %v err %v", ok, err)
	}
}
