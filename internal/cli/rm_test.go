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
