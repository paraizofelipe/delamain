package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/store"
)

const deadPID = 999_999

// fixture is a state dir with a real repository and worktrees, as a run leaves them.
type fixture struct {
	cfgFile, stateDir, wtDir, repo string
	st                             *store.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{stateDir: filepath.Join(root, "state"), wtDir: filepath.Join(root, "worktrees"), repo: filepath.Join(root, "app")}
	f.cfgFile = filepath.Join(root, "config.yaml")
	cfg := "paths:\n  state_dir: " + f.stateDir + "\n  worktrees_dir: " + f.wtDir + "\n"
	if err := os.WriteFile(f.cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", f.repo},
		{"-C", f.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "chore: init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(f.stateDir, "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.st = st
	return f
}

// run seeds a run of the fixture's repo, with its worktree under worktrees_dir.
func (f *fixture) run(t *testing.T, ref, branch string, state store.RunState) store.Run {
	t.Helper()
	ctx := context.Background()
	rec, err := f.st.CreateRun(ctx, f.repo, ref, "Fix "+ref)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration, rec.Branch, rec.Base = state, 1, branch, "main"
	if branch != "" {
		rec.Worktree = filepath.Join(f.wtDir, "app", branch)
		if err := gitx.New().AddWorktree(ctx, f.repo, rec.Worktree, branch, "main"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (f *fixture) exec(t *testing.T, args ...string) string {
	t.Helper()
	out, err := execute(t, append([]string{"--config", f.cfgFile}, args...)...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func TestListShouldShowOnlyActiveRunsWithTheirRunningAgents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	active := f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.run(t, "#2", "fix/2-b", store.StateRecorded)
	_ = f.st.StartAgent(ctx, store.Agent{RunID: active.ID, Role: "planner", PID: os.Getpid()})
	_ = f.st.EndAgent(ctx, active.ID, "planner", store.AgentDone)
	_ = f.st.StartAgent(ctx, store.Agent{RunID: active.ID, Role: "implementer", PID: os.Getpid()})

	for _, cmd := range []string{"ls", "list"} {
		out := f.exec(t, cmd)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "RUN") {
			t.Fatalf("%s: header + one active run:\n%s", cmd, out)
		}
		row := strings.Fields(lines[1])
		if row[0] != "1" || row[1] != "app" || row[2] != "#1" || row[3] != "implement" || row[4] != "1/3" || row[5] != "fix/1-a" || row[6] != "implementer" || !strings.Contains(lines[1], "(alive)") {
			t.Fatalf("%s: row = %q", cmd, lines[1])
		}
	}
	all := f.exec(t, "ls", "--all")
	if !strings.Contains(all, "fix/2-b") || !strings.Contains(all, "(finished)") || strings.Index(all, "fix/2-b") > strings.Index(all, "fix/1-a") {
		t.Fatalf("--all includes finished runs, newest first:\n%s", all)
	}
}

func TestListShouldFlagAnActiveRunWhoseOwnerDied(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement)
	if ok, err := f.st.ClaimRun(context.Background(), rec.ID, os.Getpid(), deadPID); err != nil || !ok {
		t.Fatal(err)
	}
	_ = f.st.StartAgent(context.Background(), store.Agent{RunID: rec.ID, Role: "implementer", PID: os.Getpid()})
	out := f.exec(t, "ls")
	if !strings.Contains(out, "dead: run delamain recover") {
		t.Fatalf("a dead owner must be visible:\n%s", out)
	}
	if row := strings.Fields(strings.Split(strings.TrimSpace(out), "\n")[1]); row[6] != "-" {
		t.Fatalf("an agent recorded as running died with its owner; none is running: %v", row)
	}
}

func TestListShouldSayWhenNothingIsRunning(t *testing.T) {
	f := newFixture(t)
	if out := f.exec(t, "ls"); strings.TrimSpace(out) != "no active runs" {
		t.Fatalf("got %q", out)
	}
	var runs []RunView
	if err := json.Unmarshal([]byte(f.exec(t, "ls", "-o", "json")), &runs); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("json is an empty list, not null: %v %v", runs, err)
	}
}

func TestListShouldReportNoActiveRunsInEnglish(t *testing.T) {
	f := newFixture(t)
	if out := f.exec(t, "list"); !strings.Contains(out, "no active runs") {
		t.Fatalf("got %q", out)
	}
}

func TestStatusShouldShowTheRunItsWorktreesAndAgents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.run(t, "#2", "fix/2-b", store.StateRecorded)
	// Checkouts outside worktrees_dir are not delamain's: never listed.
	if err := gitx.New().AddWorktree(ctx, f.repo, filepath.Join(t.TempDir(), "mine"), "feat/mine", "main"); err != nil {
		t.Fatal(err)
	}
	_ = f.st.StartAgent(ctx, store.Agent{RunID: rec.ID, Role: "implementer", PID: os.Getpid(), LogPath: "/l/implementer.log"})
	_, _ = f.st.RecordUsage(ctx, rec.ID, "implementer", "/s.jsonl", store.Usage{Input: 1500, CacheRead: 9000, CostUSD: 0.25})

	out := f.exec(t, "status", "1")
	for _, want := range []string{
		"#1 Fix #1", "implement (iteration 1/3)", "fix/1-a (base main)", "(alive)", "1.5K of 2M tokens · US$ 0.25",
		"delamain logs 1 -f", "fix/2-b", "implementer  running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "feat/mine") {
		t.Errorf("only worktrees under worktrees_dir/<repo> are listed:\n%s", out)
	}
	var marked []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "*") {
			marked = append(marked, l)
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0], "fix/1-a") {
		t.Errorf("exactly the run's own worktree is marked: %q", marked)
	}
}

func TestStatusShouldPrintTheSameDataAsYAMLAndJSON(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rec := f.run(t, "#1", "fix/1-a", store.StateImplement)
	_ = f.st.StartAgent(ctx, store.Agent{RunID: rec.ID, Role: "planner", PID: os.Getpid(), SessionDir: "/s/planner"})
	_ = f.st.EndAgent(ctx, rec.ID, "planner", store.AgentDone)
	_, _ = f.st.RecordUsage(ctx, rec.ID, "planner", "/p.jsonl", store.Usage{Input: 10, Output: 5, CacheWrite: 1})

	var fromJSON, fromYAML RunStatus
	if err := json.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "json")), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(f.exec(t, "status", "1", "--output", "yaml")), &fromYAML); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]RunStatus{"json": fromJSON, "yaml": fromYAML} {
		if s.Run.ID != 1 || s.Run.Branch != "fix/1-a" || s.Run.Owner.PID != os.Getpid() || !s.Run.Owner.Alive {
			t.Errorf("%s run = %+v", name, s.Run)
		}
		if len(s.Run.Agents) != 1 || s.Run.Agents[0].Tokens != 16 || s.Run.Agents[0].EndedAt == nil || s.Run.Agents[0].Process.Alive || s.Run.Agents[0].SessionDir != "/s/planner" {
			t.Errorf("%s agents = %+v", name, s.Run.Agents)
		}
		if len(s.Worktrees) != 1 || !s.Worktrees[0].ThisRun || s.Worktrees[0].Branch != "fix/1-a" || len(s.Worktrees[0].Head) != 40 {
			t.Errorf("%s worktrees = %+v", name, s.Worktrees)
		}
		if s.Budget.Tokens != 16 || s.WorktreesDir != filepath.Join(f.wtDir, "app") {
			t.Errorf("%s budget/dir = %+v %s", name, s.Budget, s.WorktreesDir)
		}
	}
}

func TestStatusShouldExplainWhenTheRepositoryIsGone(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "", store.StateSetup)
	if err := os.RemoveAll(f.repo); err != nil {
		t.Fatal(err)
	}
	if out := f.exec(t, "status", "1"); !strings.Contains(out, "not listed:") || !strings.Contains(out, "none started") {
		t.Fatalf("a moved repository is reported, not fatal:\n%s", out)
	}
}

func TestStatusShouldRejectBadInput(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "", store.StateSetup)
	cases := map[string][]string{
		"not a number":   {"status", "abc"},
		"no argument":    {"status"},
		"unknown run":    {"status", "42"},
		"unknown format": {"status", "1", "-o", "xml"},
	}
	for name, args := range cases {
		if out, err := execute(t, append([]string{"--config", f.cfgFile}, args...)...); err == nil {
			t.Errorf("%s must fail, got:\n%s", name, out)
		}
	}
	if _, err := execute(t, "--config", f.cfgFile, "status", "#1"); err != nil {
		t.Errorf("#1 is accepted as run 1: %v", err)
	}
}

func TestWithinShouldOnlyAcceptPathsStrictlyInsideTheDir(t *testing.T) {
	cases := map[string]bool{
		"/wt/app/fix/1-a": true,
		"/wt/app":         false, // the directory itself is not one of its worktrees
		"/wt/app-other/x": false,
		"/wt":             false,
		"/elsewhere":      false,
	}
	for path, want := range cases {
		if got := within("/wt/app", path); got != want {
			t.Errorf("within(/wt/app, %s) = %v", path, got)
		}
	}
}
