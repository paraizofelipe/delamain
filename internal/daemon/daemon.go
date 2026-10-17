// Package daemon is step 12 of the worker flow: it polls forges for ready issues and runs them,
// at most MaxParallel at a time, each in its own worktree and herdr workspace.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/pipeline"
)

// Source is one repository the daemon serves.
type Source struct {
	Repo  string
	Forge forge.Forge
}

type Options struct {
	Sources     []Source
	Label       string
	MaxParallel int
	Interval    time.Duration
	Run         func(ctx context.Context, t pipeline.Task, fg forge.Forge) (pipeline.Result, error)
	// DayExhausted returns a reason when the daily budget forbids new runs.
	DayExhausted func(ctx context.Context) (string, error)
	Log          *slog.Logger
}

type Daemon struct {
	opts     Options
	slots    chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	inflight map[string]bool
	finished int
	cond     *sync.Cond
}

func New(o Options) *Daemon {
	d := &Daemon{opts: o, slots: make(chan struct{}, max(o.MaxParallel, 1)), inflight: map[string]bool{}}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// Serve polls immediately and then every Interval until ctx is cancelled, then waits for the
// runs in flight (they observe the cancellation and finish as halted, delivering drafts).
func (d *Daemon) Serve(ctx context.Context) error {
	interval := d.opts.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.Poll(ctx)
		select {
		case <-ctx.Done():
			d.Wait()
			return nil
		case <-t.C:
		}
	}
}

// Poll starts ready issues while slots are free and returns how many it started.
func (d *Daemon) Poll(ctx context.Context) int {
	if ctx.Err() != nil {
		return 0
	}
	if d.opts.DayExhausted != nil {
		reason, err := d.opts.DayExhausted(ctx)
		if err != nil {
			d.opts.Log.Error("budget check failed, not starting runs", "err", err)
			return 0
		}
		if reason != "" {
			d.opts.Log.Info("daily budget spent, waiting", "reason", reason)
			return 0
		}
	}
	started := 0
	for _, c := range d.candidates(ctx) {
		key := fmt.Sprintf("%s#%d", c.src.Repo, c.issue.Number)
		d.mu.Lock()
		busy := d.inflight[key]
		d.mu.Unlock()
		if busy {
			continue
		}
		select {
		case d.slots <- struct{}{}:
		default:
			return started // all slots busy; the rest stays ready for the next poll
		}
		d.mu.Lock()
		d.inflight[key] = true
		d.mu.Unlock()
		d.wg.Add(1)
		started++
		go d.run(ctx, key, c.src, c.issue)
	}
	return started
}

type candidate struct {
	src   Source
	issue forge.Issue
}

// candidates interleaves the repositories' ready issues (each oldest first) so one busy
// repository cannot starve the others.
func (d *Daemon) candidates(ctx context.Context) []candidate {
	var lists [][]forge.Issue
	for _, s := range d.opts.Sources {
		is, err := s.Forge.ListReady(ctx, d.opts.Label)
		if err != nil {
			d.opts.Log.Warn("could not list ready issues", "repo", s.Repo, "err", err)
		}
		lists = append(lists, is)
	}
	var out []candidate
	for i := 0; ; i++ {
		added := false
		for j, is := range lists {
			if i < len(is) {
				out = append(out, candidate{d.opts.Sources[j], is[i]})
				added = true
			}
		}
		if !added {
			return out
		}
	}
}

func (d *Daemon) run(ctx context.Context, key string, src Source, is forge.Issue) {
	defer func() {
		<-d.slots
		d.mu.Lock()
		delete(d.inflight, key)
		d.finished++
		d.cond.Broadcast()
		d.mu.Unlock()
		d.wg.Done()
	}()
	d.opts.Log.Info("run started", "issue", key)
	res, err := d.opts.Run(ctx, pipeline.TaskFromIssue(is, src.Repo), src.Forge)
	if err != nil {
		d.opts.Log.Error("run failed before recording an outcome", "issue", key, "err", err)
		return
	}
	d.opts.Log.Info("run finished", "issue", key, "outcome", res.Outcome, "reason", res.Reason, "pr", res.PRURL)
}

// Wait blocks until every started run has finished.
func (d *Daemon) Wait() { d.wg.Wait() }

// waitIdle blocks until at least n runs have finished.
func (d *Daemon) waitIdle(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.finished < n {
		d.cond.Wait()
	}
}
