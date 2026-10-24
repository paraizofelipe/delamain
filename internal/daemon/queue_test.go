package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/pipeline"
)

// queueJobs is a queue of issues of /q run by r, in order.
func queueJobs(r *runner, ns ...string) func(context.Context) ([]Job, error) {
	return func(context.Context) ([]Job, error) {
		var out []Job
		for _, n := range ns {
			out = append(out, Job{Key: "/q#" + n, Run: func(ctx context.Context) (pipeline.Result, error) {
				return r.run(ctx, pipeline.Task{Repo: "/q", Ref: "#" + n}, nil)
			}})
		}
		return out, nil
	}
}

func TestPollShouldStartTheQueueBeforeTheReadyIssues(t *testing.T) {
	r := newRunner()
	d := New(Options{
		Sources: []Source{{Repo: "/a", Forge: &stubForge{ready: issues(1)}}}, MaxParallel: 2, Log: discard,
		Queue:     queueJobs(r, "12", "21"),
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
	})
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("started %d, want 2 (max_parallel)", n)
	}
	close(r.release)
	d.Wait()
	got := r.snapshot()
	slices.Sort(got)
	if want := []string{"/q#12", "/q#21"}; !slices.Equal(got, want) {
		t.Fatalf("the queue goes first: started %v, want %v", got, want)
	}
}

func TestPollQueueShouldNotAskTheProducers(t *testing.T) {
	r := newRunner()
	asked := false
	d := New(Options{
		MaxParallel: 2, Log: discard, Sources: []Source{{Repo: "/a"}},
		Queue: queueJobs(r, "12"),
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

// slotsStub hands out free slots and counts the releases.
type slotsStub struct {
	mu       sync.Mutex
	free     int
	released int
}

func (s *slotsStub) acquire(context.Context) (func(), bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.free == 0 {
		return nil, false, nil
	}
	s.free--
	return func() {
		s.mu.Lock()
		s.released++
		s.free++
		s.mu.Unlock()
	}, true, nil
}

func TestPollShouldStopWhenNoSharedSlotIsFree(t *testing.T) {
	r := newRunner()
	slots := &slotsStub{free: 1}
	d := New(Options{MaxParallel: 3, Log: discard, Queue: queueJobs(r, "12", "21", "30"), Acquire: slots.acquire})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("started %d, want 1: the other slots are held by other processes", n)
	}
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("started %d with no free slot", n)
	}
	r.release <- struct{}{}
	d.waitIdle(1)
	slots.mu.Lock()
	released := slots.released
	slots.mu.Unlock()
	if released != 1 {
		t.Fatalf("a finished run releases its slot: %d", released)
	}
	if got := r.snapshot(); !slices.Equal(got, []string{"/q#12"}) {
		t.Fatalf("started %v, want the head of the queue", got)
	}
	select {
	case <-d.wake:
	default:
		t.Fatal("a finished run wakes the queue up")
	}
	close(r.release)
	d.Wait()
}
