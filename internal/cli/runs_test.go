package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/paraizo/delamain/internal/budget"
	"github.com/paraizo/delamain/internal/config"
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
	rec, err := f.st.CreateRun(ctx, f.repo, ref, "Fix "+ref, "omp", 3, 0, store.KindIssue)
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
		if row[0] != "1" || row[1] != "issue" || row[2] != "app" || row[3] != "#1" || row[4] != "implement" || row[5] != "1/3" || row[6] != "wait" || row[7] != "fix/1-a" || row[8] != "implementer" || !strings.Contains(lines[1], "(alive)") {
			t.Fatalf("%s: row = %q", cmd, lines[1])
		}
	}
	all := f.exec(t, "ls", "--all")
	if !strings.Contains(all, "fix/2-b") || !strings.Contains(all, "(finished)") || strings.Index(all, "fix/2-b") > strings.Index(all, "fix/1-a") {
		t.Fatalf("--all includes finished runs, newest first:\n%s", all)
	}
}

func TestListShouldShowTheComplexityOrWaitUntilThePlanClassifiesIt(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StatePlan)
	classified := f.run(t, "#2", "fix/2-b", store.StateImplement)
	classified.Complexity = "high"
	if err := f.st.UpdateRun(context.Background(), classified); err != nil {
		t.Fatal(err)
	}
	f.run(t, "#3", "fix/3-c", store.StateRecorded) // ended before any plan classified it

	out := f.exec(t, "ls", "--all")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if header := strings.Fields(lines[0]); len(header) < 7 || header[5] != "ITER" || header[6] != "COMPLEXITY" {
		t.Fatalf("COMPLEXITY follows ITER:\n%s", out)
	}
	got := map[string]string{}
	for _, l := range lines[1:] {
		row := strings.Fields(l)
		got[row[3]] = row[6]
	}
	if got["#1"] != "wait" || got["#2"] != "high" || got["#3"] != "-" {
		t.Fatalf("complexity per issue = %v, want #1 wait (still planning), #2 high, #3 - (finished unclassified):\n%s", got, out)
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
	if row := strings.Fields(strings.Split(strings.TrimSpace(out), "\n")[1]); row[8] != "-" {
		t.Fatalf("an agent recorded as running died with its owner; none is running: %v", row)
	}
}

func TestListShouldSayWhenNothingIsRunning(t *testing.T) {
	f := newFixture(t)
	if out := f.exec(t, "ls"); strings.TrimSpace(out) != "no active run" {
		t.Fatalf("got %q", out)
	}
	var runs []RunView
	if err := json.Unmarshal([]byte(f.exec(t, "ls", "-o", "json")), &runs); err != nil || runs == nil || len(runs) != 0 {
		t.Fatalf("json is an empty list, not null: %v %v", runs, err)
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
		"#1 Fix #1", "fix/1-a (base main)", "(alive)", "1.5K de 2M tokens · US$ 0.25",
		"delamain logs 1 -f", "fix/2-b", "implementer  running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "feat/mine") {
		t.Errorf("only worktrees under worktrees_dir/<repo> are listed:\n%s", out)
	}
	if got := statusLabels(out)[:6]; strings.Join(got, " ") != "RUN KIND REPO HARNESS STATE INTERACTIONS" {
		t.Errorf("HARNESS follows REPO and INTERACTIONS follows STATE, got %v:\n%s", got, out)
	}
	for label, want := range map[string]string{"HARNESS": "omp", "STATE": "implement", "INTERACTIONS": "1/3"} {
		if got := statusField(out, label); got != want {
			t.Errorf("%s = %q, want %q: STATE no longer repeats the iteration:\n%s", label, got, want, out)
		}
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

func TestStatusShouldShowTheHarnessAndMaxIterationsRecordedOnTheRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The fixture's config says omp and 3 iterations; the run was started under claude with 5.
	rec, err := f.st.CreateRun(ctx, f.repo, "#1", "Fix #1", "claude", 5, 0, store.KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration = store.StateImplement, 1
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}

	out := f.exec(t, "status", "1")
	if got := statusField(out, "HARNESS"); got != "claude" {
		t.Errorf("HARNESS = %q, want what the run was started with:\n%s", got, out)
	}
	if got := statusField(out, "INTERACTIONS"); got != "1/5" {
		t.Errorf("INTERACTIONS = %q, want the run's own maximum:\n%s", got, out)
	}
}

func TestStatusShouldFallBackForRunsWithoutHarness(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Runs created before the harness was recorded read back as "" and 0.
	rec, err := f.st.CreateRun(ctx, f.repo, "#1", "Fix #1", "", 0, 0, store.KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration = store.StateImplement, 1
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}

	out := f.exec(t, "status", "1")
	if got := statusField(out, "HARNESS"); got != "-" {
		t.Errorf("HARNESS = %q, want - for a run that never recorded it:\n%s", got, out)
	}
	if got := statusField(out, "INTERACTIONS"); got != "1/3" {
		t.Errorf("INTERACTIONS = %q, want the config's maximum as fallback:\n%s", got, out)
	}
	if row := strings.Fields(strings.Split(strings.TrimSpace(f.exec(t, "ls")), "\n")[1]); row[5] != "1/3" {
		t.Errorf("ls keeps showing x/N for old runs: %v", row)
	}
}

func TestStatusShouldShowNoInteractionBeforeTheFirstIteration(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.CreateRun(context.Background(), f.repo, "#1", "Fix #1", "claude", 5, 0, store.KindIssue); err != nil {
		t.Fatal(err)
	}
	out := f.exec(t, "status", "1")
	if got := statusField(out, "INTERACTIONS"); got != "-" {
		t.Errorf("INTERACTIONS = %q, want - before the first iteration:\n%s", got, out)
	}
	if got := statusField(out, "HARNESS"); got != "claude" {
		t.Errorf("HARNESS = %q, want claude:\n%s", got, out)
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

	// Scripts read the keys by name: harness and max_iterations are added, iteration stays.
	var rawJSON, rawYAML struct {
		Run map[string]any `json:"run" yaml:"run"`
	}
	if err := json.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "json")), &rawJSON); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "yaml")), &rawYAML); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]map[string]any{"json": rawJSON.Run, "yaml": rawYAML.Run} {
		if fmt.Sprint(run["harness"]) != "omp" || fmt.Sprint(run["max_iterations"]) != "3" || fmt.Sprint(run["iteration"]) != "1" {
			t.Errorf("%s run keys: harness=%v max_iterations=%v iteration=%v", name, run["harness"], run["max_iterations"], run["iteration"])
		}
	}
}

// statusLabels lists the labels of the status table's first block, in order.
func statusLabels(out string) []string {
	var labels []string
	for _, l := range strings.Split(out, "\n") {
		if l == "" {
			break
		}
		labels = append(labels, strings.Fields(l)[0])
	}
	return labels
}

// statusField is the value of a label in the status table; the tabwriter pads with spaces.
func statusField(out, label string) string {
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == label {
			return strings.TrimSpace(strings.TrimPrefix(l, label))
		}
	}
	return ""
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

// review seeds an active review run of the fixture's repo for PR n.
func (f *fixture) review(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	rec, err := f.st.CreateRun(ctx, f.repo, fmt.Sprintf("pr:%d", n), fmt.Sprintf("Review PR %d", n), "omp", 3, 0, store.KindReview)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration = store.StateReview, 1
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
}

func TestListShouldShowTheKindOfEachRun(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.review(t, 3)

	lines := strings.Split(strings.TrimSpace(f.exec(t, "ls")), "\n")
	if len(lines) != 3 {
		t.Fatalf("header + two active runs:\n%s", strings.Join(lines, "\n"))
	}
	if header := strings.Fields(lines[0]); header[0] != "RUN" || header[1] != "KIND" || header[2] != "REPO" {
		t.Fatalf("KIND follows RUN: %v", header)
	}
	kinds := map[string]string{}
	for _, l := range lines[1:] {
		row := strings.Fields(l)
		kinds[row[3]] = row[1]
	}
	if kinds["#1"] != "issue" || kinds["pr:3"] != "review" {
		t.Fatalf("kind by issue ref = %v:\n%s", kinds, strings.Join(lines, "\n"))
	}
}

func TestStatusShouldShowTheKindOfTheRun(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.review(t, 3)

	issue := f.exec(t, "status", "1")
	if got := statusLabels(issue)[:2]; strings.Join(got, " ") != "RUN KIND" {
		t.Errorf("KIND follows RUN, got %v:\n%s", got, issue)
	}
	if got := statusField(issue, "KIND"); got != "issue" {
		t.Errorf("KIND = %q, want issue:\n%s", got, issue)
	}
	if got := statusField(f.exec(t, "status", "2"), "KIND"); got != "review" {
		t.Errorf("KIND = %q, want review", got)
	}
}

func TestStatusShouldIncludeTheKindInJSONAndYAML(t *testing.T) {
	f := newFixture(t)
	f.review(t, 3)

	var rawJSON, rawYAML struct {
		Run map[string]any `json:"run" yaml:"run"`
	}
	if err := json.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "json")), &rawJSON); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "yaml")), &rawYAML); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]map[string]any{"json": rawJSON.Run, "yaml": rawYAML.Run} {
		if run["kind"] != "review" {
			t.Errorf("%s run kind = %v, want review", name, run["kind"])
		}
	}
	var runs []map[string]any
	if err := json.Unmarshal([]byte(f.exec(t, "ls", "-o", "json")), &runs); err != nil || len(runs) != 1 || runs[0]["kind"] != "review" {
		t.Errorf("ls json carries the kind too: %v %v", runs, err)
	}
}

func TestStatusShouldShowTheComplexityAndItsPerIssueBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := os.WriteFile(f.cfgFile, []byte("paths:\n  state_dir: "+f.stateDir+"\n  worktrees_dir: "+f.wtDir+"\nbudget:\n  per_issue: 2000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := f.st.CreateRun(ctx, f.repo, "#1", "Fix #1", "omp", 3, 2e6, store.KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	rec.State, rec.Iteration, rec.Complexity, rec.PerIssue, rec.MaxIterations = store.StateImplement, 1, "high", 4e6, 5
	if err := f.st.UpdateRun(ctx, rec); err != nil {
		t.Fatal(err)
	}
	plain, err := f.st.CreateRun(ctx, f.repo, "#2", "Fix #2", "omp", 3, 7e6, store.KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	plain.State, plain.Iteration = store.StateImplement, 1
	if err := f.st.UpdateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}

	out := f.exec(t, "status", "1")
	if got := statusField(out, "COMPLEXITY"); got != "high" {
		t.Errorf("COMPLEXITY = %q:\n%s", got, out)
	}
	if got, want := statusField(out, "BUDGET"), "de "+budget.Format(4e6, config.BudgetTokens); !strings.Contains(got, want) {
		t.Errorf("BUDGET = %q, want it to contain %q", got, want)
	}
	var s RunStatus
	if err := json.Unmarshal([]byte(f.exec(t, "status", "1", "-o", "json")), &s); err != nil {
		t.Fatal(err)
	}
	if s.Run.Complexity != "high" || s.Budget.PerIssue != 4e6 {
		t.Errorf("json = %q, %v", s.Run.Complexity, s.Budget.PerIssue)
	}

	out = f.exec(t, "status", "2")
	if got := statusField(out, "COMPLEXITY"); got != "-" {
		t.Errorf("COMPLEXITY = %q, want - for an unclassified run", got)
	}
	if err := json.Unmarshal([]byte(f.exec(t, "status", "2", "-o", "json")), &s); err != nil {
		t.Fatal(err)
	}
	if s.Budget.PerIssue != 2e6 {
		t.Errorf("per_issue = %v, want the config's", s.Budget.PerIssue)
	}
}
