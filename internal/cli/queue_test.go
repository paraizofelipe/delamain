package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// issuesForge knows the open issues of open; any other number is closed.
type issuesForge struct {
	forge.Forge
	open map[int]string
}

func (f issuesForge) Issue(_ context.Context, n int) (forge.Issue, error) {
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
	if err != nil || len(js) != 2 || js[0].Key != f.repo+"#12" {
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

func TestAcquireRunSlotShouldWaitForAFreeSlot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	prev := slotPoll
	slotPoll = 10 * time.Millisecond
	t.Cleanup(func() { slotPoll = prev })
	held, ok, err := f.st.AcquireDaemonSlot(ctx, 1)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	var out strings.Builder
	got := make(chan error, 1)
	go func() {
		release, err := acquireRunSlot(ctx, f.st, 1, &out)
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the run must wait while the slot is held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, ok, _ := f.st.AcquireDaemonSlot(ctx, 2); ok {
		t.Fatal("the daemon takes no slot while a run waits, even under the limit")
	}
	if err := f.st.ReleaseSlot(ctx, held); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the run takes the freed slot")
	}
	if n, _ := f.st.SlotsInUse(ctx); n != 0 {
		t.Fatalf("release frees the slot: %d in use", n)
	}
}
