// Package daemon is step 12 of the worker flow: it runs the issues of the queue one at a time, and
// polls forges for ready issues (and, with review on, for PRs/MRs labeled for review) and runs
// them beside it, at most MaxParallel at a time.
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
	// Queue lists the jobs of the queue, in its order. They run one at a time, the head first.
	Queue func(ctx context.Context) ([]Job, error)
	// Producers are asked, in order, for each source's jobs on every poll.
	Producers []Producer
	// Acquire takes one of the slots every delamain process shares, the queue's when queue is
	// true; ok false means none is free for it. nil bounds the runs by MaxParallel alone.
	Acquire     func(ctx context.Context, queue bool) (release func(), ok bool, err error)
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
	// queueBusy is true while an issue of the queue runs: the next one waits for it.
	queueBusy bool
	// atMax is true once a job found no free slot, so the log says it once, not on every poll.
	atMax bool
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

// start runs jobs in order while slots are free and returns how many it started. Of the queue's
// jobs only the head is a candidate, and only while no other issue of the queue runs: the queue
// is sequential, and a head already running some other way holds the rest back.
func (d *Daemon) start(ctx context.Context, jobs []Job) int {
	started, head, blocked := 0, false, false
	defer func() { d.reachedMax(blocked) }()
	for _, j := range jobs {
		d.mu.Lock()
		busy := d.inflight[j.Key] || (j.Queue && d.queueBusy)
		d.mu.Unlock()
		if j.Queue {
			if head {
				continue
			}
			head = true
		}
		if busy {
			continue
		}
		select {
		case d.slots <- struct{}{}:
		default:
			blocked = true
			return started // all slots busy; the rest stays ready for the next poll
		}
		release := func() {}
		if d.opts.Acquire != nil {
			rel, ok, err := d.opts.Acquire(ctx, j.Queue)
			if err != nil {
				d.opts.Log.Error("could not take a slot, not starting runs", "err", err)
				<-d.slots
				return started
			}
			if !ok {
				<-d.slots
				blocked = true
				if j.Queue {
					continue // the slots are all held; the other jobs follow their own rule
				}
				return started // the slots are held elsewhere or left to the queue
			}
			release = rel
		}
		d.mu.Lock()
		d.inflight[j.Key] = true
		if j.Queue {
			d.queueBusy = true
		}
		d.mu.Unlock()
		d.wg.Add(1)
		started++
		go d.run(ctx, j, release)
	}
	return started
}

// reachedMax logs once that a job found every slot held, until a poll finds one again.
func (d *Daemon) reachedMax(blocked bool) {
	if blocked && !d.atMax {
		d.opts.Log.Info("maximum parallelism reached, waiting for a free slot", "max_parallel", d.opts.MaxParallel)
	}
	d.atMax = blocked
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
		if j.Queue {
			d.queueBusy = false
		}
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
