package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// deadPID is a process id no live process has.
const deadPID = 999_999

// fixture is a repository, a worktrees dir and a store under one temp root.
type fixture struct {
	cfg  config.Config
	repo string
	st   *store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{cfg: config.Default(), repo: filepath.Join(root, "app")}
	f.cfg.Paths.StateDir = filepath.Join(root, "state")
	f.cfg.Paths.WorktreesDir = filepath.Join(root, "worktrees")
	f.cfg.Notify.Enabled = false
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", f.repo},
		{"-C", f.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(f.cfg.Paths.StateDir, "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	return f
}

// run seeds a run of the fixture's repo, owned by this process, with a worktree when branch is set.
func (f *fixture) run(t *testing.T, ref, branch string, state store.RunState, kind store.RunKind) store.Run {
	t.Helper()
	ctx := context.Background()
	rec, err := f.st.CreateRun(ctx, f.repo, ref, "Fix "+ref, "omp", 3, 0, kind)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration, rec.Branch, rec.Base = state, 1, branch, "main"
	if branch != "" {
		rec.Worktree = filepath.Join(f.cfg.Paths.WorktreesDir, "app", branch)
		if err := gitx.New().AddWorktree(ctx, f.repo, rec.Worktree, branch, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// orphan makes rec's owner a dead process.
func (f *fixture) orphan(t *testing.T, rec store.Run) {
	t.Helper()
	if ok, err := f.st.ClaimRun(context.Background(), rec.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal("seed a run whose process died", err)
	}
}

// runDir seeds the run's directory with a log, as a run leaves it.
func (f *fixture) runDir(t *testing.T, rec store.Run) string {
	t.Helper()
	dir := pipeline.RunDir(f.cfg.Paths.StateDir, rec.Repo, rec.ID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "delamain.log"), []byte("state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *fixture) get(t *testing.T, id int64) store.Run {
	t.Helper()
	rec, err := f.st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *fixture) stopRequested(t *testing.T, id int64) bool {
	t.Helper()
	stop, err := f.st.StopRequested(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return stop
}

// recorder is a notifier that keeps the toasts.
type recorder struct {
	mu     sync.Mutex
	sent   []notify.Message
	closed int
}

func (n *recorder) Notify(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, m)
	return nil
}

func (n *recorder) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed++
	return nil
}

// actions are the fixture's actions; built counts the runners made, n gets their toasts.
func (f *fixture) actions(n *recorder, built *int) *Actions {
	return New(Deps{
		Cfg: f.cfg, Store: f.st,
		NewRunner: func() (*pipeline.Runner, error) {
			*built++
			return &pipeline.Runner{
				Cfg: f.cfg, Store: f.st, Git: gitx.New(), Agents: pipeline.RPCAgents{},
				Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: n,
			}, nil
		},
	})
}

func TestStopShouldFailForAnUnknownRun(t *testing.T) {
	f := newFixture(t)
	acts := f.actions(&recorder{}, new(int))
	_, err := acts.Stop(context.Background(), 999)
	if !errors.Is(err, ErrNotFound) || err.Error() != "run 999 not found" {
		t.Fatalf("an unknown run is ErrNotFound naming it, got %v", err)
	}
	var re *RunError
	if !errors.As(err, &re) || re.ID != 999 {
		t.Fatalf("the error carries the run id: %#v", err)
	}
}

func TestStopShouldLeaveAFinishedRunAlone(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateRecorded, store.KindIssue)
	res, err := f.actions(&recorder{}, new(int)).Stop(context.Background(), rec.ID)
	if err != nil || res.Status != StopAlreadyFinished || res.State != store.StateRecorded {
		t.Fatalf("a finished run has nothing to stop: %+v %v", res, err)
	}
	if f.stopRequested(t, rec.ID) {
		t.Fatal("a finished run gets no stop request")
	}
}

func TestStopShouldRecordTheRequestForALiveOwner(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement, store.KindIssue) // owned by this test process
	res, err := f.actions(&recorder{}, new(int)).Stop(context.Background(), rec.ID)
	if err != nil || res.Status != StopRequested || res.OwnerPID != os.Getpid() || res.Recovered != nil {
		t.Fatalf("a live owner gets the request: %+v %v", res, err)
	}
	if !f.stopRequested(t, rec.ID) {
		t.Fatal("the request must be recorded for the owner to see")
	}
	if got := f.get(t, rec.ID); got.State != store.StateImplement {
		t.Fatalf("Stop never finishes a live owner's run itself: %+v", got)
	}
}

func TestStopShouldRecoverARunWhoseOwnerDied(t *testing.T) {
	f := newFixture(t)
	n, built := &recorder{}, new(int)
	rec := f.run(t, "pr:7", "review/7", store.StateReview, store.KindReview)
	other := f.run(t, "pr:8", "review/8", store.StateReview, store.KindReview)
	f.orphan(t, rec)
	f.orphan(t, other)
	acts := f.actions(n, built)

	res, err := acts.Stop(context.Background(), rec.ID)
	if err != nil || res.Status != StopRecovered || res.Recovered == nil || res.Recovered.Outcome != pipeline.OutcomeHalted {
		t.Fatalf("an orphan is finalized, never waited for: %+v %v", res, err)
	}
	if got := f.get(t, rec.ID); got.State != store.StateRecorded || !strings.HasPrefix(got.Reason, "halted:") {
		t.Fatalf("the orphan is recorded as halted: %+v", got)
	}
	if got := f.get(t, other.ID); got.State != store.StateReview {
		t.Fatalf("only the given run is recovered: %+v", got)
	}
	if len(n.sent) != 1 || n.sent[0].Event != notify.EventRecover {
		t.Fatalf("the recover toast is sent as `delamain recover` sends it: %+v", n.sent)
	}

	if _, err := acts.Stop(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	if *built != 1 {
		t.Fatalf("the runner is built once for every recover, got %d", *built)
	}
	if err := acts.Close(); err != nil || n.closed != 1 {
		t.Fatalf("Close closes the runner's notifier: %v %d", err, n.closed)
	}
}

func TestStopShouldFailOnAnOrphanWithoutARunner(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "", store.StateImplement, store.KindIssue)
	f.orphan(t, rec)
	acts := New(Deps{Cfg: f.cfg, Store: f.st})
	if _, err := acts.Stop(context.Background(), rec.ID); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("without a runner an orphan cannot be recovered here: %v", err)
	}
	if err := acts.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverShouldRefuseAFinishedRunOrALiveOwner(t *testing.T) {
	f := newFixture(t)
	acts := f.actions(&recorder{}, new(int))
	done := f.run(t, "#1", "", store.StateRecorded, store.KindIssue)
	_, err := acts.Recover(context.Background(), done.ID)
	if !errors.Is(err, ErrRunFinished) || err.Error() != "run 1 already finished (recorded)" {
		t.Fatalf("a finished run is ErrRunFinished: %v", err)
	}
	live := f.run(t, "#2", "", store.StateImplement, store.KindIssue)
	_, err = acts.Recover(context.Background(), live.ID)
	var re *RunError
	if !errors.Is(err, ErrOwnerAlive) || !errors.As(err, &re) || re.OwnerPID != os.Getpid() {
		t.Fatalf("a live owner is ErrOwnerAlive with its pid: %v", err)
	}
	if _, err := acts.Recover(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown run is ErrNotFound: %v", err)
	}
}

func TestRemoveShouldRefuseAnUnfinishedRun(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement, store.KindIssue)
	dir := f.runDir(t, rec)
	err := f.actions(&recorder{}, new(int)).Remove(context.Background(), rec.ID, false)
	if !errors.Is(err, ErrRunNotFinished) || err.Error() != "run 1 is still implement: stop it first with 'delamain stop 1'" {
		t.Fatalf("an unfinished run is refused: %v", err)
	}
	f.get(t, rec.ID)
	if !exists(dir) || !exists(rec.Worktree) {
		t.Fatal("a refused run keeps its directory and worktree")
	}
}

func TestRemoveShouldDeleteRecordDirectoryAndWorktreeButKeepTheBranch(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded, store.KindIssue)
	dir := f.runDir(t, rec)
	if err := f.actions(&recorder{}, new(int)).Remove(context.Background(), rec.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.GetRun(context.Background(), rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the record is gone: %v", err)
	}
	if exists(dir) || exists(rec.Worktree) {
		t.Fatal("the directory and the worktree are gone")
	}
	if !gitx.New().BranchExists(context.Background(), f.repo, "fix/1-a") {
		t.Fatal("the branch stays: it may be the draft the run delivered")
	}
	if err := f.actions(&recorder{}, new(int)).Remove(context.Background(), rec.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed run is not found: %v", err)
	}
}

func TestRemoveShouldRefuseADirtyWorktreeUnlessForced(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded, store.KindIssue)
	dir := f.runDir(t, rec)
	if err := os.WriteFile(filepath.Join(rec.Worktree, "wip.go"), []byte("package wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	acts := f.actions(&recorder{}, new(int))
	err := acts.Remove(context.Background(), rec.ID, false)
	var re *RunError
	if !errors.Is(err, ErrDirtyWorktree) || !errors.As(err, &re) || re.Worktree != rec.Worktree {
		t.Fatalf("uncommitted work is ErrDirtyWorktree naming the worktree: %v", err)
	}
	if want := "worktree " + rec.Worktree + " has uncommitted changes; use --force to discard them"; err.Error() != want {
		t.Fatalf("message = %q, want %q", err, want)
	}
	if !exists(filepath.Join(rec.Worktree, "wip.go")) || !exists(dir) {
		t.Fatal("a refused run keeps its worktree and directory")
	}
	if err := acts.Remove(context.Background(), rec.ID, true); err != nil {
		t.Fatalf("force discards the changes: %v", err)
	}
	if exists(rec.Worktree) || exists(dir) {
		t.Fatal("force removes the worktree and the run directory")
	}
}

func TestRemoveShouldRefuseAWorktreeAnActiveRunIsUsingEvenForced(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded, store.KindIssue)
	dir := f.runDir(t, rec)
	fix, err := f.st.CreateRun(ctx, f.repo, "addr:42", "Fix #1", "omp", 3, 0, store.KindAddress)
	if err != nil {
		t.Fatal(err)
	}
	fix.State, fix.Worktree = store.StateImplement, rec.Worktree
	if err := f.st.UpdateRun(ctx, fix); err != nil {
		t.Fatal(err)
	}
	acts := f.actions(&recorder{}, new(int))
	for _, force := range []bool{false, true} {
		err := acts.Remove(ctx, rec.ID, force)
		var re *RunError
		if !errors.Is(err, ErrWorktreeInUse) || !errors.As(err, &re) || re.UsedBy != fix.ID || re.State != store.StateImplement {
			t.Fatalf("force=%v: a worktree an active run uses is never removed: %v", force, err)
		}
		if want := "worktree " + rec.Worktree + " is in use by run 2 (implement)"; err.Error() != want {
			t.Fatalf("message = %q, want %q", err, want)
		}
		if !exists(rec.Worktree) || !exists(dir) {
			t.Fatal("a refused run keeps its worktree and directory")
		}
	}
}

func TestRecoveredResultShouldNameHowTheRunEnded(t *testing.T) {
	for _, tc := range []struct {
		res  pipeline.Result
		want string
	}{
		{pipeline.Result{PRURL: "https://x/pr/1"}, "PR opened"},
		{pipeline.Result{PRURL: "https://x/pr/1", Draft: true}, "draft PR"},
		{pipeline.Result{Outcome: pipeline.OutcomeNeedsInfo}, "needs info"},
		{pipeline.Result{Outcome: pipeline.OutcomeHalted}, "halted"},
		{pipeline.Result{Outcome: pipeline.OutcomeExhausted}, "needs review"},
	} {
		if got := RecoveredResult(tc.res); got != tc.want {
			t.Errorf("%+v → %q, want %q", tc.res, got, tc.want)
		}
	}
}
