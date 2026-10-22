package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

// sampleStatus is a run with every optional line of the status table filled in.
func sampleStatus() RunStatus {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	plannerEnd, implEnd := t0.Add(90*time.Second), t0.Add(30*time.Second)
	return RunStatus{
		Run: RunView{
			ID: 7, Kind: "issue", Repo: "/r/app", Issue: "#45", Title: "Bold labels", State: "review",
			Iteration: 2, iterations: 5, Harness: "claude", Complexity: "medium",
			Branch: "feat/45-x", Base: "main", Worktree: "/wt/app/feat/45-x", Workspace: "dm-7",
			PR: "https://github.com/o/app/pull/9", Result: "halted: guardrail tripped", RunDir: "/s/runs/7",
			Agents: []AgentView{
				{Role: "planner", State: "done", Process: Process{PID: 41}, StartedAt: t0, EndedAt: &plannerEnd, Tokens: 1500, CostUSD: 0.25},
				{Role: "implementer", State: "running", Process: Process{PID: 42, Alive: true}, StartedAt: t0, EndedAt: &implEnd},
			},
		},
		WorktreesDir: "/wt/app",
		Worktrees: []WorktreeView{
			{Worktree: gitx.Worktree{Path: "/wt/app/feat/45-x", Branch: "feat/45-x", Head: "0123456789abcdef"}, ThisRun: true},
			{Worktree: gitx.Worktree{Path: "/wt/app/fix/2-b", Head: "fedcba9876543210", Detached: true}},
		},
		Budget: BudgetView{Summary: "1.5K of 2M tokens"},
	}
}

// sampleTable is sampleStatus as the tabwriter laid it out before colors: written by hand.
const sampleTable = `RUN           7 · #45 Bold labels
KIND          issue
REPO          /r/app
HARNESS       claude
STATE         review
INTERACTIONS  2/5
COMPLEXITY    medium
BRANCH        feat/45-x (base main)
WORKTREE      /wt/app/feat/45-x
OWNER         -
BUDGET        1.5K of 2M tokens
PR/MR         https://github.com/o/app/pull/9
HERDR         dm-7
RESULT        halted: guardrail tripped
LOGS          /s/runs/7/logs (delamain logs 7 -f)

WORKTREES in /wt/app
   BRANCH      HEAD      PATH
*  feat/45-x   01234567  /wt/app/feat/45-x
   (detached)  fedcba98  /wt/app/fix/2-b

AGENTS
ROLE         STATE    PID  ALIVE  DURATION  TOKENS  US$
planner      done     41   no     1m30s     1.5K    0.25
implementer  running  42   yes    30s       0       0.00
`

func TestStatusShouldNotEmitANSIWhenNotATerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR_FORCE", "")
	var out bytes.Buffer
	if err := statusTable(&out, sampleStatus()); err != nil {
		t.Fatal(err)
	}
	if out.String() != sampleTable {
		t.Fatalf("a writer that is not a terminal gets the plain table, byte for byte:\n%q\nwant\n%q", out.String(), sampleTable)
	}
}

// forcedStatus renders s as a terminal that supports ANSI colors shows it.
func forcedStatus(t *testing.T, s RunStatus) string {
	t.Helper()
	var out bytes.Buffer
	r := lipgloss.NewRenderer(&out)
	r.SetColorProfile(termenv.ANSI)
	if err := renderStatus(&out, s, newStyles(r)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return sgr.ReplaceAllString(s, "") }

// codesOf returns the SGR parameters applied to every rune of text, in the first line of out
// whose plain text starts with prefix; found is false when that line has no such text.
func codesOf(out, prefix, text string) (codes map[string]bool, found bool) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(stripANSI(line), prefix) {
			continue
		}
		var runes []rune
		var styles [][]string
		var active []string
		for i := 0; i < len(line); {
			if loc := sgr.FindStringIndex(line[i:]); loc != nil && loc[0] == 0 {
				params := line[i+2 : i+loc[1]-1]
				if params == "" || params == "0" {
					active = nil
				} else {
					active = append(append([]string{}, active...), strings.Split(params, ";")...)
				}
				i += loc[1]
				continue
			}
			r, n := utf8.DecodeRuneInString(line[i:])
			runes, styles = append(runes, r), append(styles, active)
			i += n
		}
		at := strings.Index(string(runes), text)
		if at < 0 {
			return nil, false
		}
		start := utf8.RuneCountInString(string(runes)[:at])
		codes = map[string]bool{}
		for _, c := range styles[start] {
			codes[c] = true
		}
		for _, st := range styles[start+1 : start+utf8.RuneCountInString(text)] {
			for c := range codes {
				if !slices.Contains(st, c) {
					delete(codes, c)
				}
			}
		}
		return codes, true
	}
	return nil, false
}

// wantCodes fails unless text, in the line starting with prefix, carries exactly codes.
func wantCodes(t *testing.T, out, prefix, text string, codes ...string) {
	t.Helper()
	got, found := codesOf(out, prefix, text)
	if !found {
		t.Errorf("%q not found in the line starting with %q:\n%s", text, prefix, out)
		return
	}
	want := map[string]bool{}
	for _, c := range codes {
		want[c] = true
	}
	if !maps.Equal(got, want) {
		t.Errorf("%q in the %q line has SGR %v, want %v:\n%q", text, prefix, got, want, out)
	}
}

func TestStatusShouldBoldLabelsAndHeadings(t *testing.T) {
	out := forcedStatus(t, sampleStatus())
	for _, label := range []string{"RUN", "KIND", "REPO", "HARNESS", "STATE", "INTERACTIONS", "COMPLEXITY", "BRANCH", "WORKTREE ", "OWNER", "BUDGET", "PR/MR", "HERDR", "RESULT", "LOGS"} {
		wantCodes(t, out, label, strings.TrimSpace(label), "1")
	}
	wantCodes(t, out, "WORKTREES", "WORKTREES", "1")
	wantCodes(t, out, "WORKTREES", " in /wt/app")
	wantCodes(t, out, "AGENTS", "AGENTS", "1")
	for _, h := range []string{"BRANCH", "HEAD", "PATH"} {
		wantCodes(t, out, "   BRANCH", h, "1")
	}
	for _, h := range []string{"ROLE", "STATE", "PID", "ALIVE", "DURATION", "TOKENS", "US$"} {
		wantCodes(t, out, "ROLE", h, "1")
	}
	wantCodes(t, out, "HARNESS", "claude")
	wantCodes(t, out, "BUDGET", "1.5K of 2M tokens")
}

func TestStatusShouldKeepColumnsAlignedWithColor(t *testing.T) {
	out := forcedStatus(t, sampleStatus())
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("a forced ANSI renderer colors the table:\n%q", out)
	}
	if got := stripANSI(out); got != sampleTable {
		t.Errorf("without the escapes the colored table is the plain one:\n%s\nwant\n%s", got, sampleTable)
	}

	// No worktree of this run: the marker column shrinks to the padding alone.
	s := sampleStatus()
	s.Worktrees[0].ThisRun, s.Run.PR, s.Run.Result = false, "", ""
	want := strings.NewReplacer(
		"PR/MR         https://github.com/o/app/pull/9\n", "",
		"RESULT        halted: guardrail tripped\n", "",
		"   BRANCH      HEAD      PATH\n", "  BRANCH      HEAD      PATH\n",
		"*  feat/45-x   01234567", "  feat/45-x   01234567",
		"   (detached)  fedcba98", "  (detached)  fedcba98",
	).Replace(sampleTable)
	if got := stripANSI(forcedStatus(t, s)); got != want {
		t.Errorf("without a worktree of this run:\n%s\nwant\n%s", got, want)
	}
}

// colorEnv sets the variables that decide color for the command, isolated from the caller's.
func colorEnv(t *testing.T, force, noColor string) {
	t.Helper()
	t.Setenv("CLICOLOR_FORCE", force)
	t.Setenv("NO_COLOR", noColor)
	t.Setenv("CLICOLOR", "")
}

func TestStatusShouldColorWhenCLIColorForceIsSet(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	colorEnv(t, "", "")
	plain := f.exec(t, "status", "1")

	colorEnv(t, "1", "")
	out := f.exec(t, "status", "1")
	wantCodes(t, out, "STATE", "STATE", "1")
	wantCodes(t, out, "AGENTS", "AGENTS", "1")
	if got := stripANSI(out); got != plain {
		t.Errorf("colors add escapes, not text:\n%s\nwant\n%s", got, plain)
	}
}

func TestStatusShouldNotEmitANSIWhenNoColorIsSet(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	colorEnv(t, "1", "1") // NO_COLOR wins over CLICOLOR_FORCE
	out := f.exec(t, "status", "1")
	if strings.Contains(out, "\x1b") || !strings.Contains(out, "STATE") {
		t.Fatalf("NO_COLOR prints the plain table:\n%q", out)
	}
}

func TestStatusShouldNotEmitANSIInYAMLOrJSONWhenColorIsForced(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	colorEnv(t, "1", "")
	var s RunStatus
	jsonOut := f.exec(t, "status", "1", "-o", "json")
	if err := json.Unmarshal([]byte(jsonOut), &s); err != nil || strings.Contains(jsonOut, "\x1b") {
		t.Errorf("json stays plain: %v\n%q", err, jsonOut)
	}
	yamlOut := f.exec(t, "status", "1", "-o", "yaml")
	if err := yaml.Unmarshal([]byte(yamlOut), &s); err != nil || strings.Contains(yamlOut, "\x1b") {
		t.Errorf("yaml stays plain: %v\n%q", err, yamlOut)
	}
}

func TestStatusShouldColorComplexityByLevel(t *testing.T) {
	for level, codes := range map[string][]string{"low": {"32"}, "medium": {"33"}, "high": {"31"}, "": nil} {
		s := sampleStatus()
		s.Run.Complexity = level
		wantCodes(t, forcedStatus(t, s), "COMPLEXITY", orDash(level), codes...)
	}
}

func TestStatusShouldColorResultByOutcome(t *testing.T) {
	cases := map[string][]string{
		"approved": {"32"}, "reviewed": {"32"}, "answered": {"32"},
		"exhausted": {"33"}, "needs_info": {"33"},
		"halted":  {"31"},
		"skipped": {"2"},
		"mystery": nil, // an outcome this version does not know stays plain
	}
	for outcome, codes := range cases {
		s := sampleStatus()
		s.Run.Result = outcome
		wantCodes(t, forcedStatus(t, s), "RESULT", outcome, codes...)
	}

	// Only the outcome is colored; the reason is free text and prints as is.
	out := forcedStatus(t, sampleStatus())
	wantCodes(t, out, "RESULT", "halted", "31")
	wantCodes(t, out, "RESULT", ": guardrail tripped")
}

func TestStatusShouldColorRunState(t *testing.T) {
	cases := map[store.RunState][]string{
		store.StateRecorded: {"32"}, store.StateNeedsInfo: {"33"}, store.StateHalted: {"31"},
		store.StateImplement: {"36"}, store.StateReview: {"36"}, store.StatePreflight: {"36"},
		store.StateSetup: {"36"}, store.StateDeliver: {"36"}, store.StateDeliverDraft: {"36"},
	}
	for state, codes := range cases {
		s := sampleStatus()
		s.Run.State = string(state)
		wantCodes(t, forcedStatus(t, s), "STATE", string(state), codes...)
	}
}

func TestStatusShouldColorRunRefPRAndLogsHint(t *testing.T) {
	out := forcedStatus(t, sampleStatus())
	wantCodes(t, out, "RUN", "#45", "36")
	wantCodes(t, out, "RUN", "7 · ")
	wantCodes(t, out, "RUN", " Bold labels") // the title comes from the issue and prints as is
	wantCodes(t, out, "PR/MR", "https://github.com/o/app/pull/9", "4", "36")
	wantCodes(t, out, "LOGS", "(delamain logs 7 -f)", "2")
	wantCodes(t, out, "LOGS", "/s/runs/7/logs ")
}

func TestStatusShouldHighlightThisRunWorktree(t *testing.T) {
	out := forcedStatus(t, sampleStatus())
	wantCodes(t, out, "*", "*", "1", "36")
	wantCodes(t, out, "*", "feat/45-x", "1", "36")
	wantCodes(t, out, "*", "01234567  /wt/app/feat/45-x")
	wantCodes(t, out, "   (detached)", "(detached)")

	s := sampleStatus()
	s.Worktrees[0].ThisRun, s.Worktrees[1].ThisRun = false, true
	out = forcedStatus(t, s)
	wantCodes(t, out, "*", "(detached)", "1", "36")
	wantCodes(t, out, "   feat/45-x", "feat/45-x")
}

func TestStatusShouldColorAgentState(t *testing.T) {
	for state, codes := range map[store.AgentState][]string{store.AgentDone: {"32"}, store.AgentRunning: {"36"}, store.AgentFailed: {"31"}} {
		s := sampleStatus()
		s.Run.Agents[0].State = string(state)
		wantCodes(t, forcedStatus(t, s), "planner", string(state), codes...)
	}
}

func TestStatusShouldColorAgentAlive(t *testing.T) {
	out := forcedStatus(t, sampleStatus())
	wantCodes(t, out, "implementer", "yes", "32")
	wantCodes(t, out, "planner", "no", "2")
}

// sampleRuns are runs of different widths, with every look a row of the ls table has.
func sampleRuns() []RunView {
	now := time.Now()
	running := []AgentView{{Role: "implementer", Process: Process{PID: 42, Alive: true}}}
	return []RunView{
		{ID: 12, Kind: "issue", Repo: "/r/app", Issue: "#45", State: "implement", Iteration: 2, iterations: 5, Complexity: "low",
			Branch: "feat/45-x", Agents: running, Owner: Process{PID: 42, Alive: true}, UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: 9, Kind: "issue", Repo: "/r/app", Issue: "#46", State: "plan", Owner: Process{PID: deadPID}, UpdatedAt: now.Add(-5 * time.Minute)},
		{ID: 7, Kind: "review", Repo: "/r/backend", Issue: "pr:3", State: "recorded", Iteration: 1, iterations: 3, Complexity: "medium",
			Owner: Process{PID: 41}, terminal: true, UpdatedAt: now.Add(-50 * time.Hour)},
		{ID: 3, Kind: "issue", Repo: "/r/backend", Issue: "#2", State: "halted", Iteration: 1, iterations: 3, terminal: true, UpdatedAt: now.Add(-30 * time.Second)},
	}
}

// sampleListRows are sampleRuns as the ls table showed them before colors: written by hand.
var sampleListRows = []string{
	"RUN\tKIND\tREPO\tISSUE\tSTATE\tITER\tCOMPLEXITY\tBRANCH\tAGENTS\tOWNER\tUPDATED",
	"12\tissue\tapp\t#45\timplement\t2/5\tlow\tfeat/45-x\timplementer\tpid 42 (alive)\t3h",
	"9\tissue\tapp\t#46\tplan\t-\twait\t-\t-\tpid 999999 (dead: run delamain recover)\t5m",
	"7\treview\tbackend\tpr:3\trecorded\t1/3\tmedium\t-\t-\tpid 41 (finished)\t2d",
	"3\tissue\tbackend\t#2\thalted\t1/3\t-\t-\t-\t-\t30s",
}

// forcedList renders views as a terminal that supports ANSI colors shows `ls`.
func forcedList(t *testing.T, views []RunView) string {
	t.Helper()
	var out bytes.Buffer
	r := lipgloss.NewRenderer(&out)
	r.SetColorProfile(termenv.ANSI)
	if err := renderList(&out, views, false, newStyles(r)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestListShouldBoldTheHeader(t *testing.T) {
	out := forcedList(t, sampleRuns())
	for _, h := range []string{"RUN", "KIND", "REPO", "ISSUE", "STATE", "ITER", "COMPLEXITY", "BRANCH", "AGENTS", "OWNER", "UPDATED"} {
		wantCodes(t, out, "RUN", h, "1")
	}
}

func TestListShouldKeepColumnsAlignedWithColor(t *testing.T) {
	colorEnv(t, "", "")
	var want bytes.Buffer
	tw := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
	for _, row := range sampleListRows {
		fmt.Fprintln(tw, row)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	var plainOut bytes.Buffer
	if err := listTable(&plainOut, sampleRuns(), false); err != nil {
		t.Fatal(err)
	}
	if plainOut.String() != want.String() {
		t.Errorf("a writer that is not a terminal gets the tabwriter table, byte for byte:\n%q\nwant\n%q", plainOut.String(), want.String())
	}

	out := forcedList(t, sampleRuns())
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("a forced ANSI renderer colors the table:\n%q", out)
	}
	if got := stripANSI(out); got != plainOut.String() {
		t.Errorf("without the escapes the colored table is the plain one:\n%s\nwant\n%s", got, plainOut.String())
	}
}

func TestListShouldNotEmitANSIWhenNotATerminal(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.run(t, "#2", "fix/2-b", store.StateRecorded)
	colorEnv(t, "", "")
	for _, args := range [][]string{{"ls"}, {"ls", "--all"}} {
		if out := f.exec(t, args...); strings.Contains(out, "\x1b") || !strings.HasPrefix(out, "RUN") {
			t.Errorf("%v: a writer that is not a terminal gets the plain table:\n%q", args, out)
		}
	}
}

func TestListShouldNotEmitANSIWhenNoColorIsSet(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.run(t, "#2", "fix/2-b", store.StateRecorded)
	colorEnv(t, "1", "1") // NO_COLOR wins over CLICOLOR_FORCE
	for _, args := range [][]string{{"ls"}, {"ls", "--all"}} {
		if out := f.exec(t, args...); strings.Contains(out, "\x1b") || !strings.HasPrefix(out, "RUN") {
			t.Errorf("%v: NO_COLOR prints the plain table:\n%q", args, out)
		}
	}
}

func TestListShouldNotEmitANSIInYAMLOrJSONWhenColorIsForced(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	colorEnv(t, "1", "")
	var runs []RunView
	jsonOut := f.exec(t, "ls", "-o", "json")
	if err := json.Unmarshal([]byte(jsonOut), &runs); err != nil || len(runs) != 1 || strings.Contains(jsonOut, "\x1b") {
		t.Errorf("json stays plain: %v\n%q", err, jsonOut)
	}
	yamlOut := f.exec(t, "ls", "-o", "yaml")
	if err := yaml.Unmarshal([]byte(yamlOut), &runs); err != nil || len(runs) != 1 || strings.Contains(yamlOut, "\x1b") {
		t.Errorf("yaml stays plain: %v\n%q", err, yamlOut)
	}
}

func TestListShouldColorStateComplexityAndOwner(t *testing.T) {
	// A row with no "-" anywhere, not even in the branch, so each case below finds the cell it changes.
	base := func() RunView {
		return RunView{ID: 5, Kind: "issue", Repo: "/r/app", Issue: "#5", State: "implement", Iteration: 1, iterations: 3, Complexity: "low",
			Branch: "feature", Agents: []AgentView{{Role: "implementer", Process: Process{PID: 42, Alive: true}}},
			Owner: Process{PID: 42, Alive: true}, UpdatedAt: time.Now().Add(-3 * time.Hour)}
	}
	row := func(change func(*RunView)) string {
		v := base()
		change(&v)
		return forcedList(t, []RunView{v})
	}

	out := row(func(*RunView) {})
	if line := strings.Split(stripANSI(out), "\n")[1]; strings.Contains(line, "-") {
		t.Fatalf("the base row must have no \"-\", or the \"-\" cases below find the wrong cell: %q", line)
	}
	wantCodes(t, out, "5 ", "implement", "36")
	wantCodes(t, out, "5 ", "low", "32")
	wantCodes(t, out, "5 ", "#5", "36")
	wantCodes(t, out, "5 ", "implementer", "36")
	wantCodes(t, out, "5 ", "pid 42 (alive)", "32")
	wantCodes(t, out, "5 ", "3h", "2")
	for _, text := range []string{"5", "issue", "app", "1/3", "feature"} {
		wantCodes(t, out, "5 ", text)
	}

	for state, codes := range map[string][]string{"recorded": {"32"}, "halted": {"31"}, "plan": {"36"}} {
		wantCodes(t, row(func(v *RunView) { v.State = state }), "5 ", state, codes...)
	}
	for level, codes := range map[string][]string{"medium": {"33"}, "high": {"31"}} {
		wantCodes(t, row(func(v *RunView) { v.Complexity = level }), "5 ", level, codes...)
	}
	wantCodes(t, row(func(v *RunView) { v.Complexity = "" }), "5 ", "wait", "2")
	wantCodes(t, row(func(v *RunView) { v.Complexity, v.terminal = "", true }), "5 ", "-")

	wantCodes(t, row(func(v *RunView) { v.Owner.Alive = false }), "5 ", "pid 42 (dead: run delamain recover)", "31")
	wantCodes(t, row(func(v *RunView) { v.terminal = true }), "5 ", "pid 42 (finished)", "2")
	wantCodes(t, row(func(v *RunView) { v.Owner = Process{} }), "5 ", "-")
	wantCodes(t, row(func(v *RunView) { v.Agents = nil }), "5 ", "-")
}

// lsAge is the UPDATED column, which may tick between two invocations of ls.
var lsAge = regexp.MustCompile(`(?m)\d+[smhd]$`)

func TestListShouldColorWhenCLIColorForceIsSet(t *testing.T) {
	f := newFixture(t)
	f.run(t, "#1", "fix/1-a", store.StateImplement)
	f.run(t, "#2", "fix/2-b", store.StateRecorded)
	for _, args := range [][]string{{"ls"}, {"ls", "--all"}} {
		colorEnv(t, "", "")
		plain := f.exec(t, args...)

		colorEnv(t, "1", "")
		out := f.exec(t, args...)
		wantCodes(t, out, "RUN", "RUN", "1")
		wantCodes(t, out, "RUN", "STATE", "1")
		wantCodes(t, out, "1 ", "implement", "36")
		if got := lsAge.ReplaceAllString(stripANSI(out), "age"); got != lsAge.ReplaceAllString(plain, "age") {
			t.Errorf("%v: colors add escapes, not text:\n%s\nwant\n%s", args, got, plain)
		}
	}
	all := f.exec(t, "ls", "--all")
	wantCodes(t, all, "2 ", "recorded", "32")
}
