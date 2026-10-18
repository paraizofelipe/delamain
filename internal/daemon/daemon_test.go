package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
)

// stubForge only lists ready issues.
type stubForge struct {
	forge.Forge
	mu    sync.Mutex
	ready []forge.Issue
	err   error
}

func (s *stubForge) ListReady(context.Context, string) ([]forge.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ready), s.err
}

func issues(ns ...int) []forge.Issue {
	out := make([]forge.Issue, 0, len(ns))
	for _, n := range ns {
		out = append(out, forge.Issue{Number: n, Title: "t", Body: "- [ ] ok", Open: true})
	}
	return out
}

// runner records runs; each run blocks until released, so tests control concurrency.
type runner struct {
	mu      sync.Mutex
	started []string
	active  int
	peak    int
	release chan struct{}
	began   chan string
}

func newRunner() *runner { return &runner{release: make(chan struct{}), began: make(chan string, 16)} }

func (r *runner) run(ctx context.Context, t pipeline.Task, _ forge.Forge) (pipeline.Result, error) {
	r.mu.Lock()
	r.started = append(r.started, t.Repo+t.Ref)
	r.active++
	r.peak = max(r.peak, r.active)
	r.mu.Unlock()
	r.began <- t.Repo + t.Ref
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return pipeline.Result{Outcome: pipeline.OutcomeApproved}, nil
}

func (r *runner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

func newDaemon(r *runner, parallel int, sources ...Source) *Daemon {
	return New(Options{
		Sources: sources, Label: "delamain:ready", MaxParallel: parallel, Run: r.run,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestPollShouldStartReadyIssuesOldestFirstUpToMaxParallel(t *testing.T) {
	r := newRunner()
	d := newDaemon(r, 2, Source{Repo: "/a", Forge: &stubForge{ready: issues(3, 5, 9)}})
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("started %d, want 2 (max_parallel)", n)
	}
	<-r.began
	<-r.began
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#3", "/a#5"}) && !slices.Equal(got, []string{"/a#5", "/a#3"}) {
		t.Fatalf("the two oldest issues run first, got %v", got)
	}
	close(r.release)
	d.Wait()
	if r.peak > 2 {
		t.Fatalf("peak concurrency %d exceeds max_parallel", r.peak)
	}
}

func TestPollShouldNotRestartAnIssueThatIsStillRunning(t *testing.T) {
	r := newRunner()
	src := &stubForge{ready: issues(3)} // label not yet removed by the run
	d := newDaemon(r, 2, Source{Repo: "/a", Forge: src})
	d.Poll(context.Background())
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("issue #3 is in flight, second poll started %d", n)
	}
	close(r.release)
	d.Wait()
	if got := r.snapshot(); len(got) != 1 {
		t.Fatalf("runs = %v", got)
	}
}

func TestPollShouldPickUpTheNextIssueOnceASlotFrees(t *testing.T) {
	r := newRunner()
	src := &stubForge{ready: issues(1, 2)}
	d := newDaemon(r, 1, Source{Repo: "/a", Forge: src})
	d.Poll(context.Background())
	r.release <- struct{}{} // #1 finishes
	d.waitIdle(1)
	src.mu.Lock()
	src.ready = issues(2) // the finished run relabeled #1
	src.mu.Unlock()
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("freed slot must take #2, started %d", n)
	}
	close(r.release)
	d.Wait()
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#1", "/a#2"}) {
		t.Fatalf("runs = %v", got)
	}
}

func TestPollShouldInterleaveRepositories(t *testing.T) {
	r := newRunner()
	d := newDaemon(r, 2,
		Source{Repo: "/a", Forge: &stubForge{ready: issues(1, 2, 3)}},
		Source{Repo: "/b", Forge: &stubForge{ready: issues(7)}},
	)
	d.Poll(context.Background())
	close(r.release)
	d.Wait()
	got := r.snapshot()
	if !slices.Contains(got, "/b#7") {
		t.Fatalf("one busy repo must not starve the others: %v", got)
	}
}

func TestPollShouldSkipAFailingForgeAndKeepServingTheOthers(t *testing.T) {
	r := newRunner()
	d := newDaemon(r, 2,
		Source{Repo: "/a", Forge: &stubForge{err: errors.New("HTTP 502")}},
		Source{Repo: "/b", Forge: &stubForge{ready: issues(4)}},
	)
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("started %d", n)
	}
	close(r.release)
	d.Wait()
}

func TestPollShouldStartNothingWhenTheDailyBudgetIsSpent(t *testing.T) {
	r := newRunner()
	d := newDaemon(r, 2, Source{Repo: "/a", Forge: &stubForge{ready: issues(1)}})
	d.opts.DayExhausted = func(context.Context) (string, error) { return "budget diário esgotado", nil }
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("started %d with the day's budget spent", n)
	}
}

func TestServeShouldWaitForRunsAfterCancellation(t *testing.T) {
	r := newRunner()
	d := newDaemon(r, 1, Source{Repo: "/a", Forge: &stubForge{ready: issues(1)}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	<-r.began
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != 0 {
		t.Fatal("Serve must not return while runs are still executing")
	}
}
