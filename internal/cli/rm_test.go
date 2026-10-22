package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/pipeline"
	"github.com/paraizo/delamain/internal/store"
)

// runDir seeds the run's directory with a log, as a run leaves it.
func (f *fixture) runDir(t *testing.T, rec store.Run) string {
	t.Helper()
	dir := pipeline.RunDir(f.stateDir, rec.Repo, rec.ID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", "delamain.log"), []byte("state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (f *fixture) branchExists(branch string) bool {
	return exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func TestRmShouldRefuseARunThatIsNotFinished(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement)
	dir := f.runDir(t, rec)

	out, err := execute(t, "--config", f.cfgFile, "rm", "1")
	if err == nil || !strings.Contains(out, "run 1 is still implement: stop it first with 'delamain stop 1'") {
		t.Fatalf("an unfinished run is refused: %v\n%s", err, out)
	}
	if _, err := f.st.GetRun(context.Background(), rec.ID); err != nil {
		t.Fatalf("a refused run keeps its record: %v", err)
	}
	if !exists(dir) || !exists(rec.Worktree) {
		t.Fatal("a refused run keeps its directory and worktree")
	}
}

func TestRmShouldRemoveTheRunItsDirectoryAndItsWorktree(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded)
	dir := f.runDir(t, rec)

	out := f.exec(t, "rm", "#1")
	if !strings.Contains(out, "removed run 1") {
		t.Fatalf("rm says what it removed:\n%s", out)
	}
	if _, err := f.st.GetRun(context.Background(), rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the record is gone, got %v", err)
	}
	if exists(dir) {
		t.Fatalf("the run directory %s is gone", dir)
	}
	if exists(rec.Worktree) {
		t.Fatalf("the worktree %s is gone", rec.Worktree)
	}
	if !f.branchExists("fix/1-a") {
		t.Fatal("the branch may be the delivered draft: rm never deletes it")
	}
	if all := f.exec(t, "ls", "--all"); strings.Contains(all, "fix/1-a") || !strings.Contains(all, "no run") {
		t.Fatalf("a removed run is no longer listed:\n%s", all)
	}
}

func TestRmShouldNotGiveBackTheDailyBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "", store.StateRecorded)
	if _, err := f.st.RecordUsage(ctx, rec.ID, "implementer", "/s.jsonl", store.Usage{Input: 1234}); err != nil {
		t.Fatal(err)
	}
	m := budget.Meter{Store: f.st, Cfg: config.Default().Budget}
	before, err := m.Today(ctx)
	if err != nil || before.Day != 1234 {
		t.Fatalf("the run's spend counts today: %v %v", before.Day, err)
	}

	f.exec(t, "rm", "1")
	after, err := m.Today(ctx)
	if err != nil || after.Day != 1234 {
		t.Fatalf("removing a run gives no budget back: before %v, after %v (%v)", before.Day, after.Day, err)
	}
}

func TestRmShouldRefuseADirtyWorktreeUnlessForced(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded)
	dir := f.runDir(t, rec)
	if err := os.WriteFile(filepath.Join(rec.Worktree, "wip.go"), []byte("package wip\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, "--config", f.cfgFile, "rm", "1")
	if want := "worktree " + rec.Worktree + " has uncommitted changes; use --force to discard them"; err == nil || !strings.Contains(out, want) {
		t.Fatalf("uncommitted work is not discarded without --force, want %q: %v\n%s", want, err, out)
	}
	if !exists(filepath.Join(rec.Worktree, "wip.go")) || !exists(dir) {
		t.Fatal("a refused run keeps its worktree and directory")
	}
	if _, err := f.st.GetRun(context.Background(), rec.ID); err != nil {
		t.Fatalf("a refused run keeps its record: %v", err)
	}

	if out := f.exec(t, "rm", "1", "--force"); !strings.Contains(out, "removed run 1") {
		t.Fatalf("--force discards the changes:\n%s", out)
	}
	if exists(rec.Worktree) || exists(dir) {
		t.Fatal("--force removes the worktree and the run directory")
	}
	if !f.branchExists("fix/1-a") {
		t.Fatal("the branch stays, forced or not")
	}
}

func TestRmShouldRemoveTheOthersWhenOneIDFails(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first := f.run(t, "#1", "", store.StateRecorded)
	last := f.run(t, "#2", "", store.StateNeedsInfo)

	out, err := execute(t, "--config", f.cfgFile, "rm", "1", "999", "2")
	if err == nil {
		t.Fatalf("a run that could not be removed makes rm fail (exit 1):\n%s", out)
	}
	for _, want := range []string{"removed run 1", "run 999 not found", "removed run 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("each id is handled on its own, missing %q:\n%s", want, out)
		}
	}
	for _, rec := range []store.Run{first, last} {
		if _, err := f.st.GetRun(ctx, rec.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("run %d must be removed despite the unknown one: %v", rec.ID, err)
		}
	}
}

func TestRmShouldRefuseAWorktreeAnActiveRunIsUsing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateRecorded)
	dir := f.runDir(t, rec)
	// Fixing the review threads of #1's PR reuses the worktree #1 left at the PR branch.
	fix, err := f.st.CreateRun(ctx, f.repo, "addr:42", "Fix #1", "omp", 3, 0, store.KindAddress)
	if err != nil {
		t.Fatal(err)
	}
	fix.State, fix.Worktree = store.StateImplement, rec.Worktree
	if err := f.st.UpdateRun(ctx, fix); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"rm", "1"}, {"rm", "1", "--force"}} {
		out, err := execute(t, append([]string{"--config", f.cfgFile}, args...)...)
		if want := "worktree " + rec.Worktree + " is in use by run 2 (implement)"; err == nil || !strings.Contains(out, want) {
			t.Fatalf("%v: a worktree an active run is using is never removed, want %q: %v\n%s", args, want, err, out)
		}
		if !exists(rec.Worktree) || !exists(dir) {
			t.Fatalf("%v: a refused run keeps its worktree and directory", args)
		}
		if _, err := f.st.GetRun(ctx, rec.ID); err != nil {
			t.Fatalf("%v: a refused run keeps its record: %v", args, err)
		}
	}
}
