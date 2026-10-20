package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
)

const reviewLabel = "delamain:review"

// stubForge only lists ready issues and the PRs labeled reviewLabel.
type stubForge struct {
	forge.Forge
	mu    sync.Mutex
	ready []forge.Issue
	prs   []forge.PRDetail
	err   error
}

func (s *stubForge) ListReady(context.Context, string) ([]forge.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ready), s.err
}

func (s *stubForge) ListPRs(_ context.Context, label string) ([]forge.PRDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if label != reviewLabel {
		return nil, s.err
	}
	return slices.Clone(s.prs), s.err
}

func pr(n int, head string) forge.PRDetail {
	return forge.PRDetail{Number: n, Open: true, HeadSHA: head}
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

func (r *runner) review(ctx context.Context, repo string, n int, _ forge.Forge) (pipeline.Result, error) {
	return r.run(ctx, pipeline.Task{Repo: repo, Ref: "#pr:" + strconv.Itoa(n)}, nil)
}

func (r *runner) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func newDaemon(r *runner, parallel int, sources ...Source) *Daemon {
	return New(Options{
		Sources: sources, MaxParallel: parallel, Log: discard,
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}),
	})
}

func TestPollShouldKeepProcessingReadyIssuesAsBefore(t *testing.T) {
	r := newRunner()
	a, b := &stubForge{ready: issues(1, 2, 3)}, &stubForge{ready: issues(7)}
	d := newDaemon(r, 2, Source{Repo: "/a", Forge: a}, Source{Repo: "/b", Forge: b})
	started := func(want ...string) {
		t.Helper()
		got := []string{<-r.began, <-r.began}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("started %v, want %v", got, want)
		}
	}
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("first poll started %d, want 2 (max_parallel)", n)
	}
	started("/a#1", "/b#7") // the oldest of each repository, not /a's two oldest
	r.release <- struct{}{}
	r.release <- struct{}{}
	d.waitIdle(2)
	a.mu.Lock()
	a.ready = issues(2, 3) // the finished runs relabeled their issues
	a.mu.Unlock()
	b.mu.Lock()
	b.ready = nil
	b.mu.Unlock()
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("second poll started %d, want 2", n)
	}
	started("/a#2", "/a#3")
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("issues in flight must not start again, started %d", n)
	}
	close(r.release)
	d.Wait()
}

// reviewDaemon runs issues and, with review enabled, the labeled PRs; lastHead maps "<repo>#<n>"
// to the head its last review saw.
func reviewDaemon(r *runner, parallel int, lastHead map[string]string, sources ...Source) *Daemon {
	reviews := ReviewJobs{
		Enabled: true, Label: reviewLabel, Run: r.review,
		LastHead: func(_ context.Context, repo string, n int) (string, error) {
			return lastHead[repo+"#"+strconv.Itoa(n)], nil
		},
	}
	return New(Options{
		Sources: sources, MaxParallel: parallel, Log: discard,
		Producers: Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, reviews),
	})
}

func TestPollShouldStartReviewJobsForLabeledPRs(t *testing.T) {
	r := newRunner()
	d := reviewDaemon(r, 4, nil,
		Source{Repo: "/a", Forge: &stubForge{prs: []forge.PRDetail{pr(4, "aaa"), pr(6, "bbb")}}},
		Source{Repo: "/b", Forge: &stubForge{prs: []forge.PRDetail{pr(2, "ccc")}}},
	)
	if n := d.Poll(context.Background()); n != 3 {
		t.Fatalf("started %d, want one review per labeled PR", n)
	}
	close(r.release)
	d.Wait()
	got := r.snapshot()
	slices.Sort(got)
	if want := []string{"/a#pr:4", "/a#pr:6", "/b#pr:2"}; !slices.Equal(got, want) {
		t.Fatalf("reviews = %v, want %v", got, want)
	}
}

func TestPollShouldSkipDraftPRs(t *testing.T) {
	r := newRunner()
	draft := pr(4, "aaa")
	draft.Draft = true
	d := reviewDaemon(r, 4, nil, Source{Repo: "/a", Forge: &stubForge{prs: []forge.PRDetail{draft, pr(6, "bbb")}}})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("started %d, want only the ready-for-review PR", n)
	}
	close(r.release)
	d.Wait()
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#pr:6"}) {
		t.Fatalf("a draft PR is not reviewed: %v", got)
	}
}

func TestPollShouldSkipPRWhoseHeadWasAlreadyReviewed(t *testing.T) {
	r := newRunner()
	reviewed := map[string]string{"/a#4": "aaa", "/a#6": "old"}
	d := reviewDaemon(r, 4, reviewed, Source{Repo: "/a", Forge: &stubForge{prs: []forge.PRDetail{
		pr(4, "aaa"), // head already reviewed
		pr(6, "bbb"), // new push since the last review
		pr(8, ""),    // head unknown: cannot be compared, so not skipped
	}}})
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("started %d, want the two PRs whose head was not reviewed", n)
	}
	close(r.release)
	d.Wait()
	got := r.snapshot()
	slices.Sort(got)
	if want := []string{"/a#pr:6", "/a#pr:8"}; !slices.Equal(got, want) {
		t.Fatalf("reviews = %v, want %v", got, want)
	}
}

func TestPollShouldNotStartReviewsWhenTheLastHeadCannotBeRead(t *testing.T) {
	r := newRunner()
	d := reviewDaemon(r, 4, nil,
		Source{Repo: "/a", Forge: &stubForge{ready: issues(1), prs: []forge.PRDetail{pr(4, "aaa")}}},
		Source{Repo: "/b", Forge: &stubForge{prs: []forge.PRDetail{pr(2, "ccc")}}},
	)
	unreadable := errors.New("database is locked")
	d.opts.Producers = Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{
		Enabled: true, Label: reviewLabel, Run: r.review,
		LastHead: func(_ context.Context, repo string, _ int) (string, error) {
			if repo == "/a" {
				return "", unreadable
			}
			return "", nil
		},
	})
	d.Poll(context.Background())
	close(r.release)
	d.Wait()
	got := r.snapshot()
	slices.Sort(got)
	if want := []string{"/a#1", "/b#pr:2"}; !slices.Equal(got, want) {
		t.Fatalf("without /a's cursor no /a review may start, the rest goes on: got %v, want %v", got, want)
	}
}

func TestPollShouldNotStartReviewJobsWhenReviewDisabled(t *testing.T) {
	r := newRunner()
	d := reviewDaemon(r, 4, nil, Source{Repo: "/a", Forge: &stubForge{ready: issues(1), prs: []forge.PRDetail{pr(4, "aaa")}}})
	d.opts.Producers = Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{
		Enabled: false, Label: reviewLabel, Run: r.review,
		LastHead: func(context.Context, string, int) (string, error) { return "", nil },
	})
	d.Poll(context.Background())
	close(r.release)
	d.Wait()
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#1"}) {
		t.Fatalf("with review.enabled false only issues run: %v", got)
	}
}

func TestPollShouldShareSlotsBetweenIssuesAndReviews(t *testing.T) {
	r := newRunner()
	src := &stubForge{ready: issues(1), prs: []forge.PRDetail{pr(4, "aaa")}}
	d := reviewDaemon(r, 1, nil, Source{Repo: "/a", Forge: src})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("max_parallel 1 with an issue and a PR queued started %d", n)
	}
	if got := <-r.began; got != "/a#1" {
		t.Fatalf("issues come before reviews, started %s", got)
	}
	r.release <- struct{}{}
	d.waitIdle(1)
	src.mu.Lock()
	src.ready = nil // the finished run relabeled #1
	src.mu.Unlock()
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("the freed slot must take the review, started %d", n)
	}
	close(r.release)
	d.Wait()
	if r.peak > 1 {
		t.Fatalf("peak concurrency %d exceeds max_parallel shared by issues and reviews", r.peak)
	}
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#1", "/a#pr:4"}) {
		t.Fatalf("runs = %v", got)
	}
}

func TestPollShouldNotStartTheSameReviewTwiceWhileInflight(t *testing.T) {
	r := newRunner()
	d := reviewDaemon(r, 2, nil, Source{Repo: "/a", Forge: &stubForge{prs: []forge.PRDetail{pr(4, "aaa")}}})
	d.Poll(context.Background())
	<-r.began
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("PR #4 is still under review, second poll started %d", n)
	}
	close(r.release)
	d.Wait()
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#pr:4"}) {
		t.Fatalf("runs = %v", got)
	}
}

func TestPollShouldContinueWhenOneProducerFails(t *testing.T) {
	r := newRunner()
	issueJobs := Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{})
	failing := func(context.Context, Source) ([]Job, error) { return nil, errors.New("HTTP 502") }
	d := New(Options{
		Sources:   []Source{{Repo: "/a", Forge: &stubForge{ready: issues(4)}}},
		Producers: append([]Producer{failing}, issueJobs...), MaxParallel: 2, Log: discard,
	})
	if n := d.Poll(context.Background()); n != 1 {
		t.Fatalf("a failing producer must not stop the others, started %d", n)
	}
	close(r.release)
	d.Wait()
	if got := r.snapshot(); !slices.Equal(got, []string{"/a#4"}) {
		t.Fatalf("runs = %v", got)
	}
}

func TestPollShouldSkipAllJobsWhenDailyBudgetIsExhausted(t *testing.T) {
	r := newRunner()
	other := func(_ context.Context, src Source) ([]Job, error) {
		return []Job{{Key: src.Repo + "#pr:9", Run: func(context.Context) (pipeline.Result, error) { return pipeline.Result{}, nil }}}, nil
	}
	reason := "budget diário esgotado"
	d := New(Options{
		Sources:     []Source{{Repo: "/a", Forge: &stubForge{ready: issues(1)}}},
		Producers:   append(Producers(discard, IssueJobs{Label: "delamain:ready", Run: r.run}, ReviewJobs{}), other),
		MaxParallel: 4, Log: discard,
		DayExhausted: func(context.Context) (string, error) { return reason, nil },
	})
	if n := d.Poll(context.Background()); n != 0 {
		t.Fatalf("started %d jobs with the day's budget spent", n)
	}
	reason = "" // a new day: both the issue and the other job are still pending
	if n := d.Poll(context.Background()); n != 2 {
		t.Fatalf("once the budget allows, the pending jobs start; started %d, want 2", n)
	}
	close(r.release)
	d.Wait()
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
