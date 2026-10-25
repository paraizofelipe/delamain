package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/pipeline"
)

// queueStub is a queue of issues of /q run by run, in order. Like the real queue, an issue leaves
// it when its job starts.
type queueStub struct {
	mu  sync.Mutex
	ns  []string
	run *runner
}

func newQueue(r *runner, ns ...string) *queueStub { return &queueStub{ns: ns, run: r} }

func (q *queueStub) jobs(context.Context) ([]Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Job
	for _, n := range q.ns {
		out = append(out, Job{Key: "/q#" + n, Queue: true, Run: func(ctx context.Context) (pipeline.Result, error) {
			q.mu.Lock()
			q.ns = slices.DeleteFunc(q.ns, func(s string) bool { return s == n })
			q.mu.Unlock()
			return q.run.run(ctx, pipeline.Task{Repo: "/q", Ref: "#" + n}, nil)
		}})
	}
	return out, nil
}

func TestPollShouldRunTheQueueOneIssueAtATime(t *testing.T) {
	r := newRunner()
	q := newQueue(r, "1", "2", "3", "4", "5")
	d := New(Options{MaxParallel: 3, Log: discard, Queue: q.jobs})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("started %d, want 1: the queue takes one slot, whatever max_parallel is", n)
	}
	if got := <-r.began; got != "/q#1" {
		t.Fatalf("started %s, want the head of the queue", got)
	}
	if n := d.PollQueue(context.Background()); n != 0 {
		t.Fatalf("started %d while an issue of the queue runs", n)
	}
	r.release <- struct{}{}
	d.waitIdle(1)
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d, want the next issue once the first ended", n)
	}
	if got := <-r.began; got != "/q#2" {
		t.Fatalf("started %s, want the second issue", got)
	}
	close(r.release)
	d.Wait()
}

func TestPollShouldRunReadyIssuesBesideTheQueue(t *testing.T) {
	r := newRunner()
	d := New(Options{
		Sources: []Source{{Repo: "/a", Forge: &stubForge{ready: issues(7, 8, 9)}}}, MaxParallel: 3, Log: discard,
		Queue:     newQueue(r, "1", "2").jobs,
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
	})
	if n := d.Poll(context.Background()); n != 3 {
		t.Fatalf("started %d, want the head of the queue and two ready issues", n)
	}
	close(r.release)
	d.Wait()
	got := r.snapshot()
	slices.Sort(got)
	if want := []string{"/a#7", "/a#8", "/q#1"}; !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
}

func TestPollShouldHoldTheQueueWhileItsHeadRunsSomeOtherWay(t *testing.T) {
	r := newRunner()
	d := New(Options{
		Sources: []Source{{Repo: "/q", Forge: &stubForge{ready: issues(1)}}}, MaxParallel: 3, Log: discard,
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
	})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("the ready issue starts: %d", n)
	}
	<-r.began
	d.opts.Queue = newQueue(r, "1", "2").jobs
	if n := d.PollQueue(context.Background()); n != 0 {
		t.Fatalf("started %d: the second issue does not jump the head", n)
	}
	close(r.release)
	d.Wait()
}

func TestPollQueueShouldNotAskTheProducers(t *testing.T) {
	r := newRunner()
	asked := false
	d := New(Options{
		MaxParallel: 2, Log: discard, Sources: []Source{{Repo: "/a"}},
		Queue: newQueue(r, "12").jobs,
		Producers: []Producer{func(context.Context, Source) ([]Job, error) {
			asked = true
			return nil, nil
		}},
	})
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d, want the queued issue", n)
	}
	close(r.release)
	d.Wait()
	if asked {
		t.Fatal("polling the queue must not list a forge")
	}
}

// slotsStub hands out slots by kind and counts the releases.
type slotsStub struct {
	mu       sync.Mutex
	free     int  // slots for the jobs outside the queue
	queue    bool // whether the queue's slot is free
	asked    []bool
	released int
}

func (s *slotsStub) acquire(_ context.Context, queue bool) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, queue)
	if queue && !s.queue || !queue && s.free == 0 {
		return nil, false, nil
	}
	if queue {
		s.queue = false
	} else {
		s.free--
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.released++
		if queue {
			s.queue = true
		} else {
			s.free++
		}
	}, true, nil
}

func TestPollShouldTakeTheQueueSlotForTheQueueAndStopWhenNoneIsFree(t *testing.T) {
	r := newRunner()
	slots := &slotsStub{free: 1, queue: true}
	d := New(Options{
		Sources: []Source{{Repo: "/a", Forge: &stubForge{ready: issues(7, 8)}}}, MaxParallel: 5, Log: discard,
		Queue:     newQueue(r, "1", "2").jobs,
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
		Acquire:   slots.acquire,
	})
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("started %d, want the queue's head in its slot and one ready issue", n)
	}
	slots.mu.Lock()
	asked := slices.Clone(slots.asked)
	slots.mu.Unlock()
	if want := []bool{true, false, false}; !slices.Equal(asked, want) {
		t.Fatalf("slots asked %v, want %v", asked, want)
	}
	if !d.atMax {
		t.Fatal("a job that found no slot is logged as the maximum reached")
	}
	r.release <- struct{}{}
	r.release <- struct{}{}
	d.waitIdle(2)
	slots.mu.Lock()
	released := slots.released
	slots.mu.Unlock()
	if released != 2 {
		t.Fatalf("finished runs release their slots: %d", released)
	}
	select {
	case <-d.wake:
	default:
		t.Fatal("a finished run wakes the queue up")
	}
	close(r.release)
	d.Wait()
}

func TestPollQueueShouldCheckTheBudgetOnlyWithIssuesQueued(t *testing.T) {
	r := newRunner()
	checks := 0
	var logs strings.Builder
	q := newQueue(r)
	d := New(Options{
		MaxParallel: 1, Log: slog.New(slog.NewTextHandler(&logs, nil)), Queue: q.jobs,
		DayExhausted: func(context.Context) (string, error) {
			checks++
			return "daily budget spent", nil
		},
	})
	d.PollQueue(context.Background())
	if checks != 0 {
		t.Fatalf("an empty queue reads no budget: %d checks", checks)
	}
	q.ns = []string{"1"}
	for range 3 {
		if n := d.PollQueue(context.Background()); n != 0 {
			t.Fatalf("a spent budget starts nothing: %d", n)
		}
	}
	if checks != 3 || strings.Count(logs.String(), "daily budget spent, waiting") != 1 {
		t.Fatalf("the spent budget is logged once, not on every poll: %d checks\n%s", checks, logs.String())
	}
	close(r.release)
	d.Wait()
}
