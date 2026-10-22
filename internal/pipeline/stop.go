package pipeline

import (
	"context"
	"errors"
	"time"
)

// ErrStopped is the cause of a run's context once `delamain stop` asked for it.
var ErrStopped = errors.New("parada pelo usuário (delamain stop)")

const defaultStopPoll = 2 * time.Second

// watchStop cancels ctx with ErrStopped once the store records a stop request for run id.
// `delamain stop` runs in another process, so the request is polled from the store.
// The returned stop waits for the watcher to exit.
func (r *Runner) watchStop(ctx context.Context, id int64, cancel context.CancelCauseFunc) func() {
	interval := r.StopPoll
	if interval <= 0 {
		interval = defaultStopPoll
	}
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
			stop, err := r.Store.StopRequested(ctx, id)
			if err != nil {
				r.Log.Warn("could not check for a stop request", "run", id, "err", err)
				continue
			}
			if stop {
				cancel(ErrStopped)
				return
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}
