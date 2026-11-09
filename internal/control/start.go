package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/paraizo/delamain/internal/store"
)

// Defaults of the run id discovery: how often the store is asked for the run a launch created,
// and for how long after the launch.
const (
	runIDPoll = 2 * time.Second
	runIDWait = 90 * time.Second
	// logTailBytes is how much of the launch log an Operation keeps.
	logTailBytes = 4 << 10
)

// Operations is where the asynchronous actions report.
func (a *Actions) Operations() *Operations { return a.d.Operations }

// AllowedRepos are the repositories the actions may start or queue: daemon.repos and api.repos.
func (a *Actions) AllowedRepos() []string {
	var out []string
	for _, r := range slices.Concat(a.d.Cfg.Daemon.Repos, a.d.Cfg.API.Repos) {
		if r = filepath.Clean(r); r != "." && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// checkRepo returns repo cleaned, or ErrRepoNotAllowed when it is not one of AllowedRepos.
func (a *Actions) checkRepo(repo string) (string, error) {
	repo = filepath.Clean(repo)
	if !a.d.AnyRepo && !slices.Contains(a.AllowedRepos(), repo) {
		return "", fmt.Errorf("%w: %s (add it to daemon.repos or api.repos)", ErrRepoNotAllowed, repo)
	}
	return repo, nil
}

// StartIssue starts `delamain run --issue issue --repo repo` as a process of its own and returns
// at once with the Operation that follows it. An issue with an active run is refused with
// ErrRunActive; a repository outside AllowedRepos with ErrRepoNotAllowed.
func (a *Actions) StartIssue(ctx context.Context, repo string, issue int) (Operation, error) {
	if issue <= 0 {
		return Operation{}, fmt.Errorf("%w: issue must be a positive number", ErrInvalid)
	}
	repo, err := a.checkRepo(repo)
	if err != nil {
		return Operation{}, err
	}
	return a.start(ctx, Operation{Kind: OpStartRun, Repo: repo, Issue: issue}, "#"+strconv.Itoa(issue),
		func() (string, error) { return a.d.Launcher.StartIssue(repo, issue) })
}

// StartReview starts `delamain review --pr pr --repo repo` as StartIssue starts a run. Reviews
// off in the configuration are refused with ErrReviewDisabled.
func (a *Actions) StartReview(ctx context.Context, repo string, pr int) (Operation, error) {
	if pr <= 0 {
		return Operation{}, fmt.Errorf("%w: pr must be a positive number", ErrInvalid)
	}
	if !a.d.Cfg.Review.Enabled {
		return Operation{}, ErrReviewDisabled
	}
	repo, err := a.checkRepo(repo)
	if err != nil {
		return Operation{}, err
	}
	return a.start(ctx, Operation{Kind: OpStartReview, Repo: repo, PR: pr}, "pr:"+strconv.Itoa(pr),
		func() (string, error) { return a.d.Launcher.StartReview(repo, pr) })
}

// start refuses op's ref while a run of it is active or another start of it is still launching,
// then launches it in the background. The ref stays reserved until the launched process ends:
// two requests in a row never start the same issue twice, even before its run exists.
func (a *Actions) start(ctx context.Context, op Operation, ref string, launch func() (string, error)) (Operation, error) {
	key := op.Repo + "\x00" + ref
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if id, ok := a.starting[key]; ok {
		return Operation{}, &RunError{Ref: ref, Operation: id, Err: ErrRunActive}
	}
	if err := a.noActiveRun(ctx, op.Repo, ref); err != nil {
		return Operation{}, err
	}
	op = a.d.Operations.add(op)
	a.starting[key] = op.ID
	go func() {
		a.follow(op, ref, launch)
		a.startMu.Lock()
		delete(a.starting, key)
		a.startMu.Unlock()
	}()
	return op, nil
}

// RecoverOperation finalizes run id as Recover does, but in the background: it returns at once
// with the Operation that follows it, after the same checks Recover makes.
func (a *Actions) RecoverOperation(ctx context.Context, id int64) (Operation, error) {
	rec, err := a.getRun(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if rec.State.Terminal() {
		return Operation{}, &RunError{ID: id, State: rec.State, Err: ErrRunFinished}
	}
	if rec.OwnerPID == os.Getpid() || a.d.Alive(rec.OwnerPID) {
		return Operation{}, &RunError{ID: id, State: rec.State, OwnerPID: rec.OwnerPID, Err: ErrOwnerAlive}
	}
	if _, err := a.runnerFor(); err != nil { // fail now, not in the background
		return Operation{}, &RunError{ID: id, Err: err}
	}
	op := a.d.Operations.add(Operation{Kind: OpRecover, Repo: rec.Repo, RunID: id})
	go func() {
		res, err := a.recover(a.d.Context, rec)
		a.d.Operations.finish(op.ID, statusOf(err), func(op *Operation) {
			if err != nil {
				op.Error = err.Error()
				return
			}
			op.Result = RecoveredResult(res)
			if res.RunDir != "" {
				op.LogPath = filepath.Join(res.RunDir, "logs", "delamain.log")
			}
		})
	}()
	return op, nil
}

// noActiveRun refuses repo's ref while a run of it is active.
func (a *Actions) noActiveRun(ctx context.Context, repo, ref string) error {
	active, err := a.d.Store.ActiveRun(ctx, repo, ref)
	switch {
	case err == nil:
		return &RunError{ID: active.ID, State: active.State, Ref: ref, Err: ErrRunActive}
	case errors.Is(err, store.ErrNotFound):
		return nil
	}
	return err
}

// follow runs start, finds the run it created and ends op with how it went.
func (a *Actions) follow(op Operation, ref string, start func() (string, error)) {
	ctx, cancel := context.WithCancel(a.d.Context)
	defer cancel()
	go a.findRunID(ctx, op, ref)
	path, err := start()
	cancel()
	// The store and the log are read before finish takes the operations' lock: the API and the
	// event stream read operations under it.
	tail := tailOf(path)
	var late int64
	if cur, ok := a.d.Operations.Get(op.ID); ok && cur.RunID == 0 {
		late = a.runIDSince(a.d.Context, op.Repo, ref, op.StartedAt)
	}
	a.d.Operations.finish(op.ID, statusOf(err), func(op *Operation) {
		op.LogPath, op.LogTail = path, tail
		// Only a process that ran has an exit code: a launch that failed before has none.
		switch exit, ok := errors.AsType[StartExit](err); {
		case err == nil:
			op.ExitCode = new(0)
		case ok:
			op.ExitCode = new(exit.Code)
		}
		if err != nil {
			op.Error = err.Error()
		}
		if op.RunID == 0 {
			op.RunID = late
		}
	})
}

// findRunID asks the store for the active run of repo's ref until it appears, ctx ends or the
// wait is over, and records it on the operation.
func (a *Actions) findRunID(ctx context.Context, op Operation, ref string) {
	t := time.NewTicker(a.d.RunIDPoll)
	defer t.Stop()
	deadline := time.After(a.d.RunIDWait)
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-t.C:
		}
		rec, err := a.d.Store.ActiveRun(ctx, op.Repo, ref)
		if err != nil {
			continue
		}
		a.d.Operations.update(op.ID, func(op *Operation) { op.RunID = rec.ID })
		return
	}
}

// runIDSince is the newest run of repo's ref created at or after since, in any state; 0 if none.
func (a *Actions) runIDSince(ctx context.Context, repo, ref string, since time.Time) int64 {
	runs, err := a.d.Store.RecentRuns(ctx, 50)
	if err != nil {
		return 0
	}
	for _, r := range runs {
		if r.Repo == repo && r.IssueRef == ref && !r.CreatedAt.Before(since.Truncate(time.Second)) {
			return r.ID
		}
	}
	return 0
}

func statusOf(err error) OpStatus {
	if err != nil {
		return OpFailed
	}
	return OpSucceeded
}

// tailOf is the last logTailBytes of the file at path, or "" when it cannot be read.
func tailOf(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path) //nolint:gosec // a launch log under the state dir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	if fi.Size() > logTailBytes {
		if _, err := f.Seek(fi.Size()-logTailBytes, io.SeekStart); err != nil {
			return ""
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(b)
}
