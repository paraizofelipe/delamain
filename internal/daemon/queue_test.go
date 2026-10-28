package daemon

import (
	"context"
	"errors"
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
	// wait is the Wait of each issue that has one, by number.
	wait map[string]func(ctx context.Context) (string, error)
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
		}, Wait: q.wait[n]})
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

func TestAFailedJobShouldNotWakeTheQueueUp(t *testing.T) {
	d := New(Options{MaxParallel: 1, Log: discard, Queue: func(context.Context) ([]Job, error) {
		return []Job{{Key: "/q#1", Queue: true, Run: func(context.Context) (pipeline.Result, error) {
			return pipeline.Result{}, errors.New("forge unreachable")
		}}}, nil
	}})
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d", n)
	}
	d.Wait()
	select {
	case <-d.wake:
		t.Fatal("a failed job waits for the queue's tick instead of retrying at once")
	default:
	}
}

// waitCounter is a Wait that always answers reason and counts how many times it was asked.
type waitCounter struct {
	mu     sync.Mutex
	reason string
	calls  int
}

func (w *waitCounter) wait(context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	return w.reason, nil
}

func (w *waitCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

func TestStartShouldSkipAWaitingQueueHeadAndStartTheNextItem(t *testing.T) {
	r := newRunner()
	q := newQueue(r, "1", "2")
	q.wait = map[string]func(context.Context) (string, error){"1": (&waitCounter{reason: "blocked by #5"}).wait}
	slots := &slotsStub{queue: true}
	d := New(Options{MaxParallel: 1, Log: discard, Queue: q.jobs, Acquire: slots.acquire})
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d, want the item after the waiting head", n)
	}
	if got := <-r.began; got != "/q#2" {
		t.Fatalf("started %s, want the second item", got)
	}
	slots.mu.Lock()
	asked := slices.Clone(slots.asked)
	slots.mu.Unlock()
	if want := []bool{true, true}; !slices.Equal(asked, want) {
		t.Fatalf("slots asked %v, want the queue's slot taken back by the next item", asked)
	}
	close(r.release)
	d.Wait()
}

func TestStartShouldReleaseTheSlotOfAWaitingItem(t *testing.T) {
	r := newRunner()
	q := newQueue(r, "1")
	w := &waitCounter{reason: "blocked by #5"}
	q.wait = map[string]func(context.Context) (string, error){"1": w.wait}
	slots := &slotsStub{queue: true}
	d := New(Options{MaxParallel: 1, Log: discard, Queue: q.jobs, Acquire: slots.acquire})
	if n := d.PollQueue(context.Background()); n != 0 {
		t.Fatalf("started %d, want the waiting item skipped", n)
	}
	slots.mu.Lock()
	released, free := slots.released, slots.queue
	slots.mu.Unlock()
	if released != 1 || !free {
		t.Fatalf("the queue's slot is given back: %d releases, free %v", released, free)
	}
	d.mu.Lock()
	busy, inflight := d.queueBusy, len(d.inflight)
	d.mu.Unlock()
	if len(d.slots) != 0 || busy || inflight != 0 || d.atMax {
		t.Fatalf("a waiting item holds nothing: %d slots, queue busy %v, %d in flight, at max %v", len(d.slots), busy, inflight, d.atMax)
	}
	w.mu.Lock()
	w.reason = ""
	w.mu.Unlock()
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d, want the item once it stops waiting", n)
	}
	if got := <-r.began; got != "/q#1" {
		t.Fatalf("started %s", got)
	}
	close(r.release)
	d.Wait()
}

func TestStartShouldNotCallWaitWithoutAFreeSlot(t *testing.T) {
	t.Run("the queue's slot is held elsewhere", func(t *testing.T) {
		r := newRunner()
		q := newQueue(r, "1", "2")
		w := &waitCounter{reason: "blocked by #5"}
		q.wait = map[string]func(context.Context) (string, error){"1": w.wait, "2": w.wait}
		slots := &slotsStub{queue: false}
		d := New(Options{MaxParallel: 1, Log: discard, Queue: q.jobs, Acquire: slots.acquire})
		if n := d.PollQueue(context.Background()); n != 0 {
			t.Fatalf("started %d without the queue's slot", n)
		}
		if c := w.count(); c != 0 {
			t.Fatalf("Wait asked %d times without a slot", c)
		}
		slots.mu.Lock()
		asked := slices.Clone(slots.asked)
		slots.mu.Unlock()
		if want := []bool{true}; !slices.Equal(asked, want) {
			t.Fatalf("slots asked %v, want %v", asked, want)
		}
	})
	t.Run("every slot of the process runs", func(t *testing.T) {
		r := newRunner()
		d := New(Options{
			Sources: []Source{{Repo: "/a", Forge: &stubForge{ready: issues(7)}}}, MaxParallel: 1, Log: discard,
			Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
		})
		if n := d.Poll(context.Background()); n != 1 {
			t.Fatalf("the ready issue starts: %d", n)
		}
		<-r.began
		q := newQueue(r, "1")
		w := &waitCounter{reason: "blocked by #5"}
		q.wait = map[string]func(context.Context) (string, error){"1": w.wait}
		d.opts.Queue = q.jobs
		if n := d.PollQueue(context.Background()); n != 0 {
			t.Fatalf("started %d without a free slot", n)
		}
		if c := w.count(); c != 0 {
			t.Fatalf("Wait asked %d times without a slot", c)
		}
		close(r.release)
		d.Wait()
	})
}

func TestStartShouldNotAskWaitOfAJobOutsideTheQueue(t *testing.T) {
	r := newRunner()
	w := &waitCounter{reason: "blocked by #5"}
	d := New(Options{
		Sources: []Source{{Repo: "/a"}}, MaxParallel: 1, Log: discard,
		Producers: []Producer{func(context.Context, Source) ([]Job, error) {
			return []Job{{Key: "/a#7", Wait: w.wait, Run: func(ctx context.Context) (pipeline.Result, error) {
				return r.run(ctx, pipeline.Task{Repo: "/a", Ref: "#7"}, nil)
			}}}, nil
		}},
	})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("started %d, want the job outside the queue as before", n)
	}
	if got := <-r.began; got != "/a#7" {
		t.Fatalf("started %s", got)
	}
	if c := w.count(); c != 0 {
		t.Fatalf("Wait asked %d times of a job outside the queue", c)
	}
	close(r.release)
	d.Wait()
}

func TestStartShouldTreatAWaitErrorAsWaiting(t *testing.T) {
	r := newRunner()
	q := newQueue(r, "1", "2")
	q.wait = map[string]func(context.Context) (string, error){"1": func(context.Context) (string, error) {
		return "", errors.New("could not read the dependencies")
	}}
	slots := &slotsStub{queue: true}
	var logs strings.Builder
	d := New(Options{MaxParallel: 1, Log: slog.New(slog.NewTextHandler(&logs, nil)), Queue: q.jobs, Acquire: slots.acquire})
	if n := d.PollQueue(context.Background()); n != 1 {
		t.Fatalf("started %d, want the item after the one whose Wait failed", n)
	}
	if got := <-r.began; got != "/q#2" {
		t.Fatalf("started %s, want the second item", got)
	}
	close(r.release)
	d.Wait()
	slots.mu.Lock()
	asked, released := slices.Clone(slots.asked), slots.released
	slots.mu.Unlock()
	if want := []bool{true, true}; !slices.Equal(asked, want) || released != 2 {
		t.Fatalf("the failed item gives its slot back: asked %v, %d releases", asked, released)
	}
	var warns []string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, `msg="queue item waiting"`) {
			warns = append(warns, l)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "level=WARN") || !strings.Contains(warns[0], "job=/q#1") ||
		!strings.Contains(warns[0], "could not read the dependencies") {
		t.Fatalf("want one warning with the job and the error, got %q\n%s", warns, logs.String())
	}
}
