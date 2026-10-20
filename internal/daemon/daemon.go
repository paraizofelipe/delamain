// Package daemon is step 12 of the worker flow: it polls forges for ready issues (and, with review
// on, for PRs/MRs labeled for review) and runs them, at most MaxParallel at a time.
package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/paraizo/delamain/internal/forge"
)

// Source is one repository the daemon serves.
type Source struct {
	Repo  string
	Forge forge.Forge
}

type Options struct {
	Sources []Source
	// Producers are asked, in order, for each source's jobs on every poll.
	Producers   []Producer
	MaxParallel int
	Interval    time.Duration
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

// Poll starts ready jobs while slots are free and returns how many it started.
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
	for _, j := range d.jobs(ctx) {
		d.mu.Lock()
		busy := d.inflight[j.Key]
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
		d.inflight[j.Key] = true
		d.mu.Unlock()
		d.wg.Add(1)
		started++
		go d.run(ctx, j)
	}
	return started
}

// jobs asks each producer, in order, for every source's jobs. Within a producer the sources are
// interleaved so one busy repository cannot starve the others; a failing producer or forge is
// skipped until the next poll.
func (d *Daemon) jobs(ctx context.Context) []Job {
	var out []Job
	for _, p := range d.opts.Producers {
		var lists [][]Job
		for _, s := range d.opts.Sources {
			js, err := p(ctx, s)
			if err != nil {
				d.opts.Log.Warn("could not list jobs", "repo", s.Repo, "err", err)
			}
			lists = append(lists, js)
		}
		for i := 0; ; i++ {
			added := false
			for _, js := range lists {
				if i < len(js) {
					out = append(out, js[i])
					added = true
				}
			}
			if !added {
				break
			}
		}
	}
	return out
}

func (d *Daemon) run(ctx context.Context, j Job) {
	defer func() {
		<-d.slots
		d.mu.Lock()
		delete(d.inflight, j.Key)
		d.finished++
		d.cond.Broadcast()
		d.mu.Unlock()
		d.wg.Done()
	}()
	d.opts.Log.Info("run started", "job", j.Key)
	res, err := j.Run(ctx)
	if err != nil {
		d.opts.Log.Error("run failed before recording an outcome", "job", j.Key, "err", err)
		return
	}
	d.opts.Log.Info("run finished", "job", j.Key, "outcome", res.Outcome, "reason", res.Reason, "pr", res.PRURL)
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
