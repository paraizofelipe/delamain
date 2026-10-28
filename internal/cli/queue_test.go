package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muesli/termenv"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/daemon"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// issuesForge knows the open issues of open; any other number is closed, unless errs fails it.
// blockers are the native "blocked by" relations of each issue.
type issuesForge struct {
	forge.Forge
	open     map[int]string
	errs     map[int]error
	blockers map[int][]forge.Blocker
}

func (f issuesForge) Blockers(_ context.Context, n int) ([]forge.Blocker, error) {
	return f.blockers[n], nil
}

func (f issuesForge) Issue(_ context.Context, n int) (forge.Issue, error) {
	if err := f.errs[n]; err != nil {
		return forge.Issue{}, err
	}
	title, ok := f.open[n]
	return forge.Issue{Number: n, Title: title, Open: ok}, nil
}

func (f *fixture) withForge(t *testing.T, open map[int]string) {
	t.Helper()
	prev := queueForge
	queueForge = func(context.Context, config.Config, string) (forge.Forge, error) { return issuesForge{open: open}, nil }
	t.Cleanup(func() { queueForge = prev })
}

func (f *fixture) queued(t *testing.T) string {
	t.Helper()
	items, err := f.st.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, fmt.Sprintf("%d:#%d", it.Position, it.Issue))
	}
	return strings.Join(out, " ")
}

func TestQueueAddShouldAppendOpenIssuesInOrder(t *testing.T) {
	f := newFixture(t)
	f.withForge(t, map[int]string{12: "Fix login", 21: "Add export", 30: "Bump deps"})
	out := f.exec(t, "queue", "add", "12", "21", "30", "--repo", f.repo)
	if !strings.Contains(out, "#30 added at position 3") || !strings.Contains(out, "only moves with delamain daemon") {
		t.Fatalf("add says where each issue went and that no daemon runs:\n%s", out)
	}
	if got := f.queued(t); got != "1:#12 2:#21 3:#30" {
		t.Fatalf("queue: %s", got)
	}
	ls := f.exec(t, "queue", "ls")
	for _, want := range []string{"POS", "#12", "Fix login", "app"} {
		if !strings.Contains(ls, want) {
			t.Fatalf("ls misses %q:\n%s", want, ls)
		}
	}
}

func TestQueueAddShouldRefuseClosedQueuedAndRunningIssues(t *testing.T) {
	f := newFixture(t)
	f.withForge(t, map[int]string{12: "a", 21: "b"})
	f.exec(t, "queue", "add", "12", "--repo", f.repo)
	f.run(t, "#21", "", store.StateImplement)
	out, err := execute(t, "--config", f.cfgFile, "queue", "add", "12", "21", "40", "--repo", f.repo)
	if err == nil {
		t.Fatalf("a refused issue fails the command:\n%s", out)
	}
	for _, want := range []string{"already in the queue", "already has active run", "issue #40 is closed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output misses %q:\n%s", want, out)
		}
	}
	if got := f.queued(t); got != "1:#12" {
		t.Fatalf("queue: %s", got)
	}
}

func TestQueueChangeShouldSwapTwoPositions(t *testing.T) {
	f := newFixture(t)
	f.withForge(t, map[int]string{12: "a", 21: "b", 30: "c"})
	f.exec(t, "queue", "add", "12", "21", "30", "--repo", f.repo)
	out := f.exec(t, "queue", "change", "1", "3")
	if !strings.Contains(out, "#12 ↔ #30 (positions 1 and 3)") {
		t.Fatalf("change says what moved:\n%s", out)
	}
	if got := f.queued(t); got != "1:#30 2:#21 3:#12" {
		t.Fatalf("queue: %s", got)
	}
	if _, err := execute(t, "--config", f.cfgFile, "queue", "change", "1", "4"); err == nil {
		t.Fatal("a position outside the queue is refused")
	}
}

func TestQueueRmShouldRemoveByPosition(t *testing.T) {
	f := newFixture(t)
	f.withForge(t, map[int]string{12: "a", 21: "b", 30: "c"})
	f.exec(t, "queue", "add", "12", "21", "30", "--repo", f.repo)
	out := f.exec(t, "queue", "rm", "1", "3")
	if !strings.Contains(out, "removed #30 from position 3") || !strings.Contains(out, "removed #12 from position 1") {
		t.Fatalf("rm says what it removed:\n%s", out)
	}
	if got := f.queued(t); got != "1:#21" {
		t.Fatalf("queue: %s", got)
	}
	if out, err := execute(t, "--config", f.cfgFile, "queue", "rm", "5"); err == nil || !strings.Contains(out, "position 5") {
		t.Fatalf("a missing position fails: %v\n%s", err, out)
	}
	if out := f.exec(t, "queue", "rm", "1"); !strings.Contains(out, "removed #21") {
		t.Fatal(out)
	}
	if out := f.exec(t, "queue", "ls"); !strings.Contains(out, "queue is empty") {
		t.Fatalf("empty queue:\n%s", out)
	}
}

func TestQueueJobShouldLeaveTheQueueOnlyOnceItsRunIsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withForge(t, map[int]string{12: "a", 21: "b"})
	for _, n := range []int{12, 21} {
		if _, err := f.st.Enqueue(ctx, f.repo, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	var queuedAtRun string
	jobs := queueJobs(config.Default(), f.st, func(_ context.Context, tk pipeline.Task, _ forge.Forge) (pipeline.Result, error) {
		queuedAtRun = f.queued(t)
		if tk.Issue != 12 || tk.Repo != f.repo {
			t.Errorf("task: %+v", tk)
		}
		return pipeline.Result{RunID: 7, Outcome: pipeline.OutcomeApproved}, nil
	})
	js, err := jobs(ctx)
	if err != nil || len(js) != 2 || js[0].Key != f.repo+"#12" || !js[0].Queue {
		t.Fatalf("one job per item, in order: %+v %v", js, err)
	}
	if _, err := js[0].Run(ctx); err != nil {
		t.Fatal(err)
	}
	if queuedAtRun != "1:#12 2:#21" {
		t.Fatalf("the issue is still queued until its run is recorded: %q", queuedAtRun)
	}
	if got := f.queued(t); got != "1:#21" {
		t.Fatalf("a recorded run takes it out, even if the runner did not: %q", got)
	}
	if res, err := js[0].Run(ctx); err != nil || res.Outcome != pipeline.OutcomeSkipped {
		t.Fatalf("an item no longer queued is not run again: %+v %v", res, err)
	}
}

func TestRunQueuedShouldKeepTheIssueWhenItsRunIsNeverRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it, err := f.st.Enqueue(ctx, f.repo, 12, "")
	if err != nil {
		t.Fatal(err)
	}
	open := issuesForge{open: map[int]string{12: "a"}}
	_, err = runQueued(ctx, f.st, open, it, func(context.Context, pipeline.Task, forge.Forge) (pipeline.Result, error) {
		return pipeline.Result{}, errors.New("repository: not a git repository")
	})
	if err == nil || f.queued(t) != "1:#12" {
		t.Fatalf("a failure before the run is recorded keeps the issue queued: %v %q", err, f.queued(t))
	}
	res, err := runQueued(ctx, f.st, open, it, func(context.Context, pipeline.Task, forge.Forge) (pipeline.Result, error) {
		return pipeline.Result{Outcome: pipeline.OutcomeNeedsInfo, Reason: "faltam critérios de aceite"}, nil
	})
	if err != nil || res.Outcome != pipeline.OutcomeNeedsInfo || f.queued(t) != "" {
		t.Fatalf("an issue the preflight rejects leaves the queue, or it would hold it: %+v %v %q", res, err, f.queued(t))
	}
}

func TestLeaveQueueOnCreateShouldTakeTheIssueOutWhenTheRunIsRecorded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.st.Enqueue(ctx, f.repo, 12, ""); err != nil {
		t.Fatal(err)
	}
	var r pipeline.Runner
	leaveQueueOnCreate(&r, f.st, pipeline.Task{Issue: 12, Ref: "#12", Repo: f.repo}, t.Logf)
	if got := f.queued(t); got != "1:#12" {
		t.Fatalf("nothing changes before the run is recorded: %q", got)
	}
	r.Created(ctx, store.Run{ID: 1})
	if got := f.queued(t); got != "" {
		t.Fatalf("the recorded run takes the issue out of the queue: %q", got)
	}
}

func TestQueueJobsShouldNotLetAnItemWithoutForgeBlockTheOthers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, it := range []struct {
		repo  string
		issue int
	}{{"/nowhere", 1}, {f.repo, 12}} {
		if _, err := f.st.Enqueue(ctx, it.repo, it.issue, ""); err != nil {
			t.Fatal(err)
		}
	}
	prev := queueForge
	queueForge = func(_ context.Context, _ config.Config, dir string) (forge.Forge, error) {
		if dir == "/nowhere" {
			return nil, nil
		}
		return issuesForge{open: map[int]string{12: "a"}}, nil
	}
	t.Cleanup(func() { queueForge = prev })
	js, err := queueJobs(config.Default(), f.st, nil)(ctx)
	if err == nil || len(js) != 1 || js[0].Key != f.repo+"#12" {
		t.Fatalf("the item without a forge waits, the next one does not: %+v %v", js, err)
	}
}

func TestAcquireRunSlotShouldRefuseARunWithNoSlotFreeForIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	release, err := acquireRunSlot(ctx, f.st, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Enqueue(ctx, f.repo, 12, ""); err != nil {
		t.Fatal(err)
	}
	_, err = acquireRunSlot(ctx, f.st, 2)
	if !errors.Is(err, errNoSlot) || err.Error() != "maximum parallelism reached: 1/2 slots in use (1 reserved for the queue)" {
		t.Fatalf("the last slot is the queue's: %v", err)
	}
	release()
	if u, _ := f.st.SlotUsage(ctx, nil); u.InUse != 0 {
		t.Fatalf("release frees the slot: %+v", u)
	}
	release, err = acquireRunSlot(ctx, f.st, 2)
	if err != nil {
		t.Fatalf("with one slot free besides the queue's the run starts: %v", err)
	}
	release()
}

func TestRunShouldExitWithCode5WhenTheMaximumParallelismIsReached(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.Enqueue(context.Background(), f.repo, 12, ""); err != nil {
		t.Fatal(err)
	}
	task := filepath.Join(t.TempDir(), "task.md")
	if err := os.WriteFile(task, []byte("# Fix login\n\n- [ ] the login works\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// max_parallel is 1 by default: the queue keeps the only slot.
	out, err := execute(t, "--config", f.cfgFile, "run", "--task", task, "--repo", f.repo)
	if code, ok := IsExit(err); !ok || code != exitNoSlot {
		t.Fatalf("exit %v, want %d:\n%s", err, exitNoSlot, out)
	}
	if !strings.Contains(out, "maximum parallelism reached: 0/1 slots in use (1 reserved for the queue)") {
		t.Fatalf("the run says why it did not start:\n%s", out)
	}
	if runs, _ := f.st.RecentRuns(context.Background(), 10); len(runs) != 0 {
		t.Fatalf("a refused run records nothing: %+v", runs)
	}
}

func TestRunQueuedShouldKeepTheIssueQueuedWhenTheForgeFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it, err := f.st.Enqueue(ctx, f.repo, 12, "")
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	run := func(context.Context, pipeline.Task, forge.Forge) (pipeline.Result, error) {
		ran = true
		return pipeline.Result{}, nil
	}
	down := issuesForge{errs: map[int]error{12: &forge.APIError{Status: 502, Message: "bad gateway"}}}
	if _, err := runQueued(ctx, f.st, down, it, run); err == nil || ran {
		t.Fatalf("a forge that fails starts nothing: %v ran=%v", err, ran)
	}
	if got := f.queued(t); got != "1:#12" {
		t.Fatalf("the issue waits in the queue for the next poll: %q", got)
	}
}

func TestRunQueuedShouldDropAClosedOrMissingIssueWithoutRunningIt(t *testing.T) {
	for _, c := range []struct {
		name string
		fg   issuesForge
		want string
	}{
		{"closed", issuesForge{}, "issue #12 is closed; removed from the queue"},
		{"missing", issuesForge{errs: map[int]error{12: &forge.APIError{Status: 404}}}, "issue #12 not found; removed from the queue"},
	} {
		f := newFixture(t)
		ctx := context.Background()
		it, err := f.st.Enqueue(ctx, f.repo, 12, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.Enqueue(ctx, f.repo, 21, ""); err != nil {
			t.Fatal(err)
		}
		res, err := runQueued(ctx, f.st, c.fg, it, func(context.Context, pipeline.Task, forge.Forge) (pipeline.Result, error) {
			t.Fatalf("%s: a closed or missing issue is not run", c.name)
			return pipeline.Result{}, nil
		})
		if err != nil || res.Outcome != pipeline.OutcomeSkipped || res.Reason != c.want {
			t.Fatalf("%s: %+v %v", c.name, res, err)
		}
		if got := f.queued(t); got != "1:#21" {
			t.Fatalf("%s: it leaves the queue so the next one can run: %q", c.name, got)
		}
	}
}

// queueDaemon is a daemon over the fixture's queue served by fg; it records the issues run starts.
type queueDaemon struct {
	*daemon.Daemon
	mu  sync.Mutex
	ran []int
}

func (f *fixture) queueDaemon(t *testing.T, fg issuesForge, issues ...int) *queueDaemon {
	t.Helper()
	for _, n := range issues {
		if _, err := f.st.Enqueue(context.Background(), f.repo, n, ""); err != nil {
			t.Fatal(err)
		}
	}
	prev := queueForge
	queueForge = func(context.Context, config.Config, string) (forge.Forge, error) { return fg, nil }
	t.Cleanup(func() { queueForge = prev })
	prevEvery := queueWaitEvery
	queueWaitEvery = 0 // every poll asks the forge
	t.Cleanup(func() { queueWaitEvery = prevEvery })
	q := &queueDaemon{}
	run := func(_ context.Context, tk pipeline.Task, _ forge.Forge) (pipeline.Result, error) {
		q.mu.Lock()
		q.ran = append(q.ran, tk.Issue)
		q.mu.Unlock()
		return pipeline.Result{RunID: int64(tk.Issue), Outcome: pipeline.OutcomeApproved}, nil
	}
	q.Daemon = daemon.New(daemon.Options{MaxParallel: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Queue: queueJobs(config.Default(), f.st, run)})
	return q
}

// poll polls the queue once and waits for what it started.
func (q *queueDaemon) poll(t *testing.T) (started int, ran []int) {
	t.Helper()
	started = q.PollQueue(context.Background())
	q.Wait()
	q.mu.Lock()
	defer q.mu.Unlock()
	return started, slices.Clone(q.ran)
}

func (f *fixture) waiting(t *testing.T) map[int]string {
	t.Helper()
	items, err := f.st.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]string{}
	for _, it := range items {
		out[it.Issue] = it.Waiting
	}
	return out
}

func TestQueueWaitShouldKeepAnIssueWithOpenDependenciesQueued(t *testing.T) {
	f := newFixture(t)
	fg := issuesForge{
		open:     map[int]string{7: "base", 12: "a", 21: "b"},
		blockers: map[int][]forge.Blocker{12: {{Number: 7, Ref: "#7", Open: true}}},
	}
	q := f.queueDaemon(t, fg, 12, 21)
	started, ran := q.poll(t)
	if started != 1 || !slices.Equal(ran, []int{21}) {
		t.Fatalf("the blocked head is not run (no comment, no label) and the next item runs: started %d ran %v", started, ran)
	}
	if got := f.queued(t); got != "1:#12" {
		t.Fatalf("the blocked issue stays in the queue: %q", got)
	}
	if got := f.waiting(t)[12]; got != "#7 (link)" {
		t.Fatalf("the queue records what it waits for: %q", got)
	}
}

func TestQueueWaitShouldReportAForgeThatCannotBeRead(t *testing.T) {
	f := newFixture(t)
	fg := issuesForge{
		open: map[int]string{21: "b"},
		errs: map[int]error{12: &forge.APIError{Status: 502, Message: "bad gateway"}},
	}
	q := f.queueDaemon(t, fg, 12, 21)
	items, err := f.st.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetQueueWaiting(context.Background(), items[0].ID, "#7 (link)"); err != nil {
		t.Fatal(err)
	}
	wait := queueJobs(config.Default(), f.st, nil)
	js, err := wait(context.Background())
	if err != nil || len(js) != 2 || js[0].Wait == nil {
		t.Fatalf("every queue job says whether it waits: %+v %v", js, err)
	}
	if reason, err := js[0].Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "bad gateway") || reason != "" {
		t.Fatalf("a forge that cannot be read is an error: %q %v", reason, err)
	}
	started, ran := q.poll(t)
	if started != 1 || !slices.Equal(ran, []int{21}) {
		t.Fatalf("the unreadable item waits and the next one runs: started %d ran %v", started, ran)
	}
	if got := f.queued(t); got != "1:#12" {
		t.Fatalf("the unreadable issue stays in the queue: %q", got)
	}
	if got := f.waiting(t)[12]; got != "#7 (link)" {
		t.Fatalf("a failed read leaves the recorded reason alone: %q", got)
	}
}

func TestQueueWaitShouldReleaseTheIssueOnceDependenciesClose(t *testing.T) {
	f := newFixture(t)
	fg := issuesForge{
		open:     map[int]string{7: "base", 12: "a"},
		blockers: map[int][]forge.Blocker{12: {{Number: 7, Ref: "#7", Open: true}}},
	}
	q := f.queueDaemon(t, fg, 12)
	if started, ran := q.poll(t); started != 0 || len(ran) != 0 || f.waiting(t)[12] != "#7 (link)" {
		t.Fatalf("the blocked issue waits: started %d ran %v waiting %q", started, ran, f.waiting(t)[12])
	}
	fg.blockers[12] = []forge.Blocker{{Number: 7, Ref: "#7", Open: false}}
	delete(fg.open, 7)
	js, err := queueJobs(config.Default(), f.st, nil)(context.Background())
	if err != nil || len(js) != 1 {
		t.Fatalf("jobs: %+v %v", js, err)
	}
	if reason, err := js[0].Wait(context.Background()); reason != "" || err != nil {
		t.Fatalf("a closed blocker holds nothing: %q %v", reason, err)
	}
	if got := f.waiting(t)[12]; got != "" {
		t.Fatalf("the reason is cleared once nothing blocks the issue: %q", got)
	}
	if started, ran := q.poll(t); started != 1 || !slices.Equal(ran, []int{12}) {
		t.Fatalf("the next poll runs it: started %d ran %v", started, ran)
	}
	if got := f.queued(t); got != "" {
		t.Fatalf("its run takes it out of the queue: %q", got)
	}
}

func TestQueueWaitShouldNotAskTheForgeAgainWithinThePause(t *testing.T) {
	f := newFixture(t)
	fg := issuesForge{
		open:     map[int]string{7: "base", 12: "a"},
		blockers: map[int][]forge.Blocker{12: {{Number: 7, Ref: "#7", Open: true}}},
	}
	q := f.queueDaemon(t, fg, 12)
	queueWaitEvery = time.Hour
	if started, _ := q.poll(t); started != 0 {
		t.Fatalf("the blocked issue waits: started %d", started)
	}
	fg.blockers[12] = []forge.Blocker{{Number: 7, Ref: "#7", Open: false}}
	delete(fg.open, 7)
	if started, ran := q.poll(t); started != 0 || len(ran) != 0 {
		t.Fatalf("within the pause the forge is not asked: started %d ran %v", started, ran)
	}
	queueWaitEvery = 0
	if started, ran := q.poll(t); started != 1 || !slices.Equal(ran, []int{12}) {
		t.Fatalf("after the pause the issue runs: started %d ran %v", started, ran)
	}
}

func TestQueueWaitShouldNotWaitForAClosedIssue(t *testing.T) {
	blocked := map[int][]forge.Blocker{12: {{Number: 7, Ref: "#7", Open: true}}}
	for _, c := range []struct {
		name string
		fg   issuesForge
	}{
		{"closed", issuesForge{open: map[int]string{7: "base"}, blockers: blocked}},
		{"missing", issuesForge{open: map[int]string{7: "base"}, blockers: blocked, errs: map[int]error{12: &forge.APIError{Status: 404}}}},
	} {
		f := newFixture(t)
		q := f.queueDaemon(t, c.fg, 12)
		js, err := queueJobs(config.Default(), f.st, nil)(context.Background())
		if err != nil || len(js) != 1 {
			t.Fatalf("%s: jobs: %+v %v", c.name, js, err)
		}
		if reason, err := js[0].Wait(context.Background()); reason != "" || err != nil {
			t.Fatalf("%s: an issue that will never run does not wait: %q %v", c.name, reason, err)
		}
		if got := f.waiting(t)[12]; got != "" {
			t.Fatalf("%s: no reason is recorded: %q", c.name, got)
		}
		if started, ran := q.poll(t); started != 1 || len(ran) != 0 {
			t.Fatalf("%s: its job starts and is not run: started %d ran %v", c.name, started, ran)
		}
		if got := f.queued(t); got != "" {
			t.Fatalf("%s: the job removes it from the queue: %q", c.name, got)
		}
	}
}

func TestRenderQueueShouldShowWhatEachItemWaitsFor(t *testing.T) {
	var out strings.Builder
	items := []store.QueueItem{
		{Position: 1, Repo: "/r/app", Issue: 12, Title: "Fix login", AddedAt: time.Now(), Waiting: "#7 (link),\x1b[2J\nacme/other#90 (link)"},
		{Position: 2, Repo: "/r/app", Issue: 21, Title: "Add export", AddedAt: time.Now()},
	}
	if err := renderQueue(&out, items, noColors()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("one line per item and the header:\n%s", out.String())
	}
	header := strings.Fields(lines[0])
	if !slices.Equal(header, []string{"POS", "ISSUE", "REPO", "ADDED", "WAITING", "TITLE"}) {
		t.Fatalf("WAITING sits between ADDED and TITLE: %q", header)
	}
	if strings.ContainsAny(out.String(), "\x1b") || !strings.Contains(lines[1], "#7 (link),[2J acme/other#90 (link)") {
		t.Fatalf("the reason is shown without control characters:\n%q", lines[1])
	}
	col := strings.Index(lines[0], "WAITING")
	if !strings.HasPrefix(lines[2][col:], "- ") {
		t.Fatalf("an item that does not wait shows -:\n%s", out.String())
	}
}

func TestRenderQueueShouldPaintWaitingItemsYellowAndSayTheyWereSkipped(t *testing.T) {
	var out strings.Builder
	items := []store.QueueItem{
		{Position: 1, Repo: "/r/app", Issue: 12, Title: "Fix login", AddedAt: time.Now().Add(-5 * time.Minute), Waiting: "#7 (link)\x1b[2J"},
		{Position: 2, Repo: "/r/app", Issue: 21, Title: "Add export", AddedAt: time.Now().Add(-5 * time.Minute)},
	}
	if err := renderQueue(&out, items, lineStyles(termenv.ANSI)); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"1", "#12", "app", "5m", "⏳ pulada · aguardando #7 (link)[2J", "Fix login"} {
		wantCodes(t, out.String(), "1 ", text, "33")
	}
	if plain := stripANSI(out.String()); strings.Contains(plain, "\x1b") || !strings.Contains(plain, "⏳ pulada · aguardando #7 (link)[2J") {
		t.Fatalf("the waiting item says it was skipped, without control characters:\n%q", out.String())
	}
}

func TestRenderQueueShouldKeepReadyItemsUnchanged(t *testing.T) {
	added := time.Now().Add(-5 * time.Minute)
	ready := store.QueueItem{Position: 2, Repo: "/r/app", Issue: 21, Title: "Add export", AddedAt: added}
	var mixed strings.Builder
	items := []store.QueueItem{
		{Position: 1, Repo: "/r/app", Issue: 12, Title: "Fix login", AddedAt: added, Waiting: "#7 (link)"},
		ready,
	}
	if err := renderQueue(&mixed, items, lineStyles(termenv.ANSI)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(mixed.String(), "\n")
	if !strings.Contains(lines[1], "⏳") || strings.Contains(lines[2], "⏳") {
		t.Fatalf("only the waiting item is marked:\n%s", mixed.String())
	}
	wantCodes(t, mixed.String(), "2 ", "2")
	wantCodes(t, mixed.String(), "2 ", "#21", "36")
	wantCodes(t, mixed.String(), "2 ", "app")
	wantCodes(t, mixed.String(), "2 ", "5m", "2")
	wantCodes(t, mixed.String(), "2 ", "-")
	wantCodes(t, mixed.String(), "2 ", "Add export")

	var alone strings.Builder
	if err := renderQueue(&alone, []store.QueueItem{ready}, lineStyles(termenv.ANSI)); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[1mPOS\x1b[0m  \x1b[1mISSUE\x1b[0m  \x1b[1mREPO\x1b[0m  \x1b[1mADDED\x1b[0m  \x1b[1mWAITING\x1b[0m  \x1b[1mTITLE\x1b[0m\n" +
		"2    \x1b[36m#21\x1b[0m    app   \x1b[2m5m\x1b[0m     -        Add export\n"
	if alone.String() != want {
		t.Fatalf("a queue of ready items prints as before:\n%q\nwant\n%q", alone.String(), want)
	}
}
