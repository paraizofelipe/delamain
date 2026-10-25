package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// issuesForge knows the open issues of open; any other number is closed, unless errs fails it.
type issuesForge struct {
	forge.Forge
	open map[int]string
	errs map[int]error
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

func TestQueueJobShouldLeaveTheQueueBeforeItRuns(t *testing.T) {
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
		return pipeline.Result{Outcome: pipeline.OutcomeApproved}, nil
	})
	js, err := jobs(ctx)
	if err != nil || len(js) != 2 || js[0].Key != f.repo+"#12" || !js[0].Queue {
		t.Fatalf("one job per item, in order: %+v %v", js, err)
	}
	if _, err := js[0].Run(ctx); err != nil {
		t.Fatal(err)
	}
	if queuedAtRun != "1:#21" {
		t.Fatalf("the issue leaves the queue as it becomes a run: %q", queuedAtRun)
	}
	if res, err := js[0].Run(ctx); err != nil || res.Outcome != pipeline.OutcomeSkipped {
		t.Fatalf("an item no longer queued is not run again: %+v %v", res, err)
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
