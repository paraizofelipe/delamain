package control

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// StopStatus says what Stop did to the run.
type StopStatus string

const (
	// StopRequested: the owner is alive and will stop the run when it sees the request.
	StopRequested StopStatus = "requested"
	// StopRecovered: the owner was dead, so the run was finalized here instead.
	StopRecovered StopStatus = "recovered"
	// StopAlreadyFinished: the run was terminal; nothing was done.
	StopAlreadyFinished StopStatus = "already_finished"
)

// StopResult is what Stop did.
type StopResult struct {
	RunID    int64
	Status   StopStatus
	State    store.RunState // the run's state when Stop looked
	OwnerPID int
	// Recovered is how the run ended, for StopRecovered.
	Recovered *pipeline.Result
}

// Stop asks the owner of run id to stop it: the request is recorded in the store and the owner
// cancels the run when it polls it. A finished run is left alone; one whose owner died is
// finalized right away, as Recover does.
func (a *Actions) Stop(ctx context.Context, id int64) (StopResult, error) {
	rec, err := a.getRun(ctx, id)
	if err != nil {
		return StopResult{}, err
	}
	res := StopResult{RunID: id, State: rec.State, OwnerPID: rec.OwnerPID}
	if rec.State.Terminal() {
		res.Status = StopAlreadyFinished
		return res, nil
	}
	if rec.OwnerPID != os.Getpid() && !a.d.Alive(rec.OwnerPID) {
		recovered, err := a.recover(ctx, rec)
		if err != nil {
			return res, err
		}
		res.Status, res.Recovered = StopRecovered, &recovered
		return res, nil
	}
	if err := a.d.Store.RequestStop(ctx, id); err != nil {
		return res, fmt.Errorf("run %d: %w", id, err)
	}
	res.Status = StopRequested
	return res, nil
}

// Recover finalizes run id, whose owner died: what was committed is delivered as a draft, the
// issue relabeled and the run recorded as halted. A finished run or one whose owner is alive is
// refused.
func (a *Actions) Recover(ctx context.Context, id int64) (pipeline.Result, error) {
	rec, err := a.getRun(ctx, id)
	if err != nil {
		return pipeline.Result{}, err
	}
	if rec.State.Terminal() {
		return pipeline.Result{}, &RunError{ID: id, State: rec.State, Err: ErrRunFinished}
	}
	if rec.OwnerPID == os.Getpid() || a.d.Alive(rec.OwnerPID) {
		return pipeline.Result{}, &RunError{ID: id, State: rec.State, OwnerPID: rec.OwnerPID, Err: ErrOwnerAlive}
	}
	return a.recover(ctx, rec)
}

// recover runs the pipeline's recovery of rec and sends the recover toast, as `delamain
// recover` does for the runs it finds.
func (a *Actions) recover(ctx context.Context, rec store.Run) (pipeline.Result, error) {
	r, err := a.runnerFor()
	if err != nil {
		return pipeline.Result{}, &RunError{ID: rec.ID, Err: err}
	}
	var forgeFor func(repo string) (forge.Forge, error)
	if a.d.ForgeFor != nil {
		forgeFor = func(repo string) (forge.Forge, error) { return a.d.ForgeFor(ctx, repo) }
	}
	res, err := r.RecoverRun(ctx, rec.ID, forgeFor)
	if err != nil {
		a.release(ctx, rec)
		return res, err
	}
	if r.Notify != nil {
		_ = r.Notify.Notify(ctx, notify.FormatRecover([]notify.Recovered{{Repo: rec.Repo, Ref: rec.IssueRef, Result: RecoveredResult(res)}}))
	}
	return res, nil
}

// release gives an unfinished run that this process claimed back to its dead owner, after a
// recovery that failed half way, such as when the client of a stop hung up: left with this live
// process as its owner, no stop or recover could finalize it again until this process ended.
func (a *Actions) release(ctx context.Context, rec store.Run) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	cur, err := a.d.Store.GetRun(ctx, rec.ID)
	if err != nil || cur.State.Terminal() || cur.OwnerPID != os.Getpid() {
		return
	}
	_, _ = a.d.Store.ClaimRun(ctx, rec.ID, os.Getpid(), rec.OwnerPID)
}

// RecoveredResult says how a recovered run ended, in the recover toast's words.
func RecoveredResult(res pipeline.Result) string {
	switch {
	case res.PRURL != "" && !res.Draft:
		return "PR opened"
	case res.PRURL != "":
		return "draft PR"
	case res.Outcome == pipeline.OutcomeNeedsInfo:
		return "needs info"
	case res.Outcome == pipeline.OutcomeHalted:
		return "halted"
	}
	return "needs review" // exhausted, or approved without a PR, as the needs_human toast
}
