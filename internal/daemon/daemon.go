// Package daemon is step 12 of the worker flow: it runs the issues of the queue, then polls forges
// for ready issues (and, with review on, for PRs/MRs labeled for review) and runs them, at most
// MaxParallel at a time.
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
	// Queue lists the jobs of the queue, in its order; they start before any producer's.
	Queue func(ctx context.Context) ([]Job, error)
	// Producers are asked, in order, for each source's jobs on every poll.
	Producers []Producer
	// Acquire takes one of the slots every delamain process shares; ok false means none is free.
	// nil bounds the runs by MaxParallel alone.
	Acquire     func(ctx context.Context) (release func(), ok bool, err error)
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
	// wake is signalled when a run ends: its slot may start the next one of the queue.
	wake chan struct{}
}

// queueInterval paces the queue between polls: it reads the state database only, never a forge,
// so it picks up new items and the slots other processes free without spending API calls.
const queueInterval = 5 * time.Second

func New(o Options) *Daemon {
	d := &Daemon{opts: o, slots: make(chan struct{}, max(o.MaxParallel, 1)), inflight: map[string]bool{}, wake: make(chan struct{}, 1)}
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
	q := time.NewTicker(queueInterval)
	defer q.Stop()
	d.Poll(ctx)
	for {
		select {
		case <-ctx.Done():
			d.Wait()
			return nil
		case <-t.C:
			d.Poll(ctx)
		case <-q.C:
			d.PollQueue(ctx)
		case <-d.wake:
			d.PollQueue(ctx)
		}
	}
}

// Poll starts the jobs of the queue, then the producers' ready ones, while slots are free and
// returns how many it started.
func (d *Daemon) Poll(ctx context.Context) int {
	if !d.mayStart(ctx) {
		return 0
	}
	return d.start(ctx, append(d.queue(ctx), d.jobs(ctx)...))
}

// PollQueue starts only the jobs of the queue; it never asks a producer, so no forge is listed.
func (d *Daemon) PollQueue(ctx context.Context) int {
	if d.opts.Queue == nil || !d.mayStart(ctx) {
		return 0
	}
	return d.start(ctx, d.queue(ctx))
}

// mayStart says whether new runs may start: not after ctx ends nor once the day's budget is spent.
func (d *Daemon) mayStart(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if d.opts.DayExhausted != nil {
		reason, err := d.opts.DayExhausted(ctx)
		if err != nil {
			d.opts.Log.Error("budget check failed, not starting runs", "err", err)
			return false
		}
		if reason != "" {
			d.opts.Log.Info("daily budget spent, waiting", "reason", reason)
			return false
		}
	}
	return true
}

// queue lists the jobs of the queue; a queue that cannot be read starts nothing until next time.
func (d *Daemon) queue(ctx context.Context) []Job {
	if d.opts.Queue == nil {
		return nil
	}
	js, err := d.opts.Queue(ctx)
	if err != nil {
		d.opts.Log.Warn("could not read the queue", "err", err)
	}
	return js
}

// start runs jobs in order while slots are free and returns how many it started.
func (d *Daemon) start(ctx context.Context, jobs []Job) int {
	started := 0
	for _, j := range jobs {
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
		release := func() {}
		if d.opts.Acquire != nil {
			rel, ok, err := d.opts.Acquire(ctx)
			if err != nil {
				d.opts.Log.Error("could not take a slot, not starting runs", "err", err)
			}
			if err != nil || !ok {
				<-d.slots
				return started // the slots are held elsewhere, or a `run` waits for the next one
			}
			release = rel
		}
		d.mu.Lock()
		d.inflight[j.Key] = true
		d.mu.Unlock()
		d.wg.Add(1)
		started++
		go d.run(ctx, j, release)
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

func (d *Daemon) run(ctx context.Context, j Job, release func()) {
	defer func() {
		release()
		<-d.slots
		d.mu.Lock()
		delete(d.inflight, j.Key)
		d.finished++
		d.cond.Broadcast()
		d.mu.Unlock()
		select {
		case d.wake <- struct{}{}:
		default: // a wake-up is already pending
		}
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
