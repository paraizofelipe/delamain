package control

import (
	"errors"
	"fmt"

	"github.com/paraizo/delamain/internal/store"
)

// Why an action refused a run. Each comes wrapped in a RunError that names the run.
var (
	ErrNotFound       = errors.New("run not found")
	ErrRunFinished    = errors.New("run already finished")
	ErrRunNotFinished = errors.New("run has not finished")
	ErrOwnerAlive     = errors.New("the owner of the run is alive")
	// ErrDirtyWorktree reads after "worktree <path> ", as the CLI and the TUI compose it.
	ErrDirtyWorktree = errors.New("has uncommitted changes; use --force to discard them")
	ErrWorktreeInUse = errors.New("worktree is in use by another run")
	ErrOutsideState  = errors.New("run directory is outside the state dir")
	// ErrNoRunner means the host gave no NewRunner, so an orphan cannot be recovered here.
	ErrNoRunner = errors.New("no runner to recover an orphaned run")
)

// RunError is an action refused on one run: Err says why, the other fields what the message
// and the API need to say so.
type RunError struct {
	ID       int64
	State    store.RunState // the run's state, when it matters
	OwnerPID int            // the alive owner, for ErrOwnerAlive
	Worktree string         // the worktree at issue, for ErrDirtyWorktree and ErrWorktreeInUse
	UsedBy   int64          // the run using the worktree, for ErrWorktreeInUse
	Err      error
}

func (e *RunError) Unwrap() error { return e.Err }

func (e *RunError) Error() string {
	switch {
	case errors.Is(e.Err, ErrNotFound):
		return fmt.Sprintf("run %d not found", e.ID)
	case errors.Is(e.Err, ErrRunFinished):
		return fmt.Sprintf("run %d already finished (%s)", e.ID, e.State)
	case errors.Is(e.Err, ErrRunNotFinished):
		return fmt.Sprintf("run %d is still %s: stop it first with 'delamain stop %d'", e.ID, e.State, e.ID)
	case errors.Is(e.Err, ErrOwnerAlive):
		return fmt.Sprintf("run %d is not orphaned: its owner (pid %d) is alive; stop it instead", e.ID, e.OwnerPID)
	case errors.Is(e.Err, ErrDirtyWorktree):
		return fmt.Sprintf("worktree %s %v", e.Worktree, e.Err)
	case errors.Is(e.Err, ErrWorktreeInUse):
		return fmt.Sprintf("worktree %s is in use by run %d (%s)", e.Worktree, e.UsedBy, e.State)
	}
	return fmt.Sprintf("run %d: %v", e.ID, e.Err)
}
