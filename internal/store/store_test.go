package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "state", "delamain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateRunShouldStartClaimedAndBeRetrievable(t *testing.T) {
	s := openTest(t)
	r, err := s.CreateRun(context.Background(), "gh:me/app", "#12", "fix login", "omp", 3, KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.State != StateClaimed || r.Iteration != 0 {
		t.Fatalf("unexpected new run: %+v", r)
	}
	got, err := s.GetRun(context.Background(), r.ID)
	if err != nil || got.Title != "fix login" {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestCreateRunShouldPersistHarnessAndMaxIterations(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, err := s.CreateRun(ctx, "repo", "#1", "a", "claude", 5, KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.Harness != "claude" || got.MaxIterations != 5 {
		t.Fatalf("status must report the harness and max iterations the run started with: %+v %v", got, err)
	}
	got.State, got.Iteration = StateImplement, 2
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.GetRun(ctx, r.ID); after.Harness != "claude" || after.MaxIterations != 5 {
		t.Fatalf("updating a run must keep what it was created with: %+v", after)
	}
}

func TestCreateRunShouldRefuseSecondActiveRunForSameIssue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue); err == nil {
		t.Fatal("two workers on the same issue would race on one branch")
	}
	if _, err := s.CreateRun(ctx, "other-repo", "#1", "a", "omp", 3, KindIssue); err != nil {
		t.Fatalf("same issue number in another repo is independent: %v", err)
	}
}

func TestCreateRunShouldAllowNewRunAfterPreviousIsRecorded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	r.State = StateRecorded
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue); err != nil {
		t.Fatalf("a finished issue may be re-run: %v", err)
	}
}

func TestUnfinishedRunsShouldListOnlyResumableRunsInOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	b, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, KindIssue)
	c, _ := s.CreateRun(ctx, "repo", "#3", "c", "omp", 3, KindIssue)
	b.State = StateNeedsInfo
	c.State, c.Iteration = StateImplement, 2
	_ = s.UpdateRun(ctx, b)
	_ = s.UpdateRun(ctx, c)

	runs, err := s.UnfinishedRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != a.ID || runs[1].ID != c.ID || runs[1].Iteration != 2 {
		t.Fatalf("want [#1 claimed, #3 implement iter 2], got %+v", runs)
	}
}

func TestUpdateRunShouldReportMissingRun(t *testing.T) {
	s := openTest(t)
	if err := s.UpdateRun(context.Background(), Run{ID: 999, State: StatePlan}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRecordUsageShouldLedgerOnlyTheDeltaBetweenSnapshots(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	f := "/sessions/implementer/s.jsonl"

	d1, err := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 100, Output: 10, CacheRead: 1000, CostUSD: 0.1})
	if err != nil || d1.Input != 100 {
		t.Fatalf("first snapshot is the whole delta: %+v %v", d1, err)
	}
	d2, _ := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 150, Output: 30, CacheRead: 1500, CostUSD: 0.25})
	if d2.Input != 50 || d2.Output != 20 {
		t.Fatalf("second read must only add the growth: %+v", d2)
	}
	d3, _ := s.RecordUsage(ctx, r.ID, "implementer", f, Usage{Input: 150, Output: 30, CacheRead: 1500, CostUSD: 0.25})
	if !d3.zero() {
		t.Fatalf("re-reading an unchanged file must not double count: %+v", d3)
	}

	total, _ := s.RunUsage(ctx, r.ID)
	if total.Input != 150 || total.Output != 30 || total.CacheRead != 1500 {
		t.Fatalf("run total = %+v", total)
	}
	if total.BudgetTokens() != 180 {
		t.Fatalf("budget tokens exclude cacheRead: got %d want 180", total.BudgetTokens())
	}
}

func TestRecordUsageShouldNotCreateNegativeEntriesWhenSnapshotShrinks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	f := "/s.jsonl"
	_, _ = s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 500})
	d, _ := s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 100}) // file rewritten
	if !d.zero() {
		t.Fatalf("shrink must reset baseline, got delta %+v", d)
	}
	d, _ = s.RecordUsage(ctx, r.ID, "planner", f, Usage{Input: 130})
	if d.Input != 30 {
		t.Fatalf("growth after reset counts from new baseline, got %+v", d)
	}
	total, _ := s.RunUsage(ctx, r.ID)
	if total.Input != 530 {
		t.Fatalf("consumed tokens are never refunded: %+v", total)
	}
}

func TestUsageSinceShouldAggregateAcrossRunsWithinWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	clock := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	a, _ := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	b, _ := s.CreateRun(ctx, "repo", "#2", "b", "omp", 3, KindIssue)
	_, _ = s.RecordUsage(ctx, a.ID, "implementer", "/a.jsonl", Usage{Input: 1000}) // yesterday
	clock = time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)
	_, _ = s.RecordUsage(ctx, a.ID, "implementer", "/a.jsonl", Usage{Input: 1200}) // +200 today
	_, _ = s.RecordUsage(ctx, b.ID, "reviewer", "/b.jsonl", Usage{Input: 300})

	today, err := s.UsageSince(ctx, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if today.Input != 500 {
		t.Fatalf("daily cap must count only today's growth across runs: got %d want 500", today.Input)
	}
}

func TestOpenShouldBeIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s1.CreateRun(ctx, "repo", "#9", "x", "omp", 3, KindIssue)
	s1.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen must not re-run migrations: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetRun(ctx, r.ID); err != nil {
		t.Fatalf("state must survive restart: %v", err)
	}
}

func TestOpenShouldRefuseDatabaseFromNewerBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	s, _ := Open(ctx, path)
	_, _ = s.db.ExecContext(ctx, "PRAGMA user_version = 99")
	s.Close()
	if _, err := Open(ctx, path); err == nil {
		t.Fatal("silently using a newer schema could corrupt state")
	}
}

func TestCreateRunShouldRecordTheOwningProcess(t *testing.T) {
	s := openTest(t)
	r, err := s.CreateRun(context.Background(), "repo", "#1", "x", "omp", 3, KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	if r.OwnerPID != os.Getpid() {
		t.Fatalf("owner pid = %d, want %d: crash recovery needs to know who runs it", r.OwnerPID, os.Getpid())
	}
}

func TestUpdateRunShouldPersistBaseAndWorkspace(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "x", "omp", 3, KindIssue)
	r.Base, r.Workspace = "origin/main", "w9"
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetRun(ctx, r.ID)
	if got.Base != "origin/main" || got.Workspace != "w9" {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenShouldUpgradeV1DatabaseKeepingRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], "PRAGMA user_version = 1",
		`INSERT INTO runs (repo, issue_ref, state, created_at, updated_at) VALUES ('repo', '#3', 'implement', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v1 → v2 must migrate: %v", err)
	}
	defer s.Close()
	runs, err := s.UnfinishedRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].OwnerPID != 0 || runs[0].IssueRef != "#3" {
		t.Fatalf("pre-v2 runs have no known owner: %+v %v", runs, err)
	}
}

func TestOpenShouldUpgradeDatabaseKeepingRunsWithEmptyHarness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], migrations[1], migrations[2], "PRAGMA user_version = 3",
		`INSERT INTO runs (repo, issue_ref, state, iteration, created_at, updated_at) VALUES ('repo', '#3', 'implement', 2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v3 → v4 must migrate: %v", err)
	}
	defer s.Close()
	runs, err := s.UnfinishedRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].IssueRef != "#3" || runs[0].Iteration != 2 {
		t.Fatalf("runs from before v4 must survive the upgrade: %+v %v", runs, err)
	}
	if runs[0].Harness != "" || runs[0].MaxIterations != 0 {
		t.Fatalf("an old run never recorded its harness, so status must fall back: %+v", runs[0])
	}
}

func TestClaimRunShouldTransferOwnershipOnlyFromTheExpectedOwner(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r, _ := s.CreateRun(ctx, "repo", "#1", "x", "omp", 3, KindIssue)
	if ok, err := s.ClaimRun(ctx, r.ID, 4242, 7); err != nil || ok {
		t.Fatalf("wrong expected owner must not claim: %v %v", ok, err)
	}
	if ok, err := s.ClaimRun(ctx, r.ID, os.Getpid(), 4242); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if ok, _ := s.ClaimRun(ctx, r.ID, os.Getpid(), 7); ok {
		t.Fatal("a second recoverer must lose the race")
	}
	if got, _ := s.GetRun(ctx, r.ID); got.OwnerPID != 4242 {
		t.Fatalf("owner = %d", got.OwnerPID)
	}
}

func TestCreateRunShouldPersistKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	issue, err := s.CreateRun(ctx, "repo", "#1", "a", "omp", 3, KindIssue)
	if err != nil {
		t.Fatal(err)
	}
	review, err := s.CreateRun(ctx, "repo", "pr:2", "b", "omp", 3, KindReview)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRun(ctx, issue.ID); got.Kind != "issue" {
		t.Fatalf("issue run kind = %q, want issue", got.Kind)
	}
	got, _ := s.GetRun(ctx, review.ID)
	if got.Kind != "review" {
		t.Fatalf("review run kind = %q, want review", got.Kind)
	}
	got.State = StateImplement
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	if after, _ := s.GetRun(ctx, review.ID); after.Kind != "review" {
		t.Fatalf("updating a run must keep the kind it was created with: %q", after.Kind)
	}
}

func TestOpenShouldUpgradeExistingRunsToIssueKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{migrations[0], migrations[1], migrations[2], migrations[3], "PRAGMA user_version = 4",
		`INSERT INTO runs (repo, issue_ref, state, iteration, created_at, updated_at) VALUES ('repo', '#3', 'implement', 2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v4 → v5 must migrate: %v", err)
	}
	defer s.Close()
	runs, err := s.UnfinishedRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].IssueRef != "#3" || runs[0].Iteration != 2 {
		t.Fatalf("runs from before v5 must survive the upgrade: %+v %v", runs, err)
	}
	if runs[0].Kind != KindIssue {
		t.Fatalf("every run before v5 worked an issue, got kind %q", runs[0].Kind)
	}
	if _, err := s.Cursor(ctx, "repo", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("v5 must create the review cursors table: %v", err)
	}
}

func TestCreateRunShouldRefuseSecondActiveReviewForSamePR(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "repo", "pr:5", "a", "omp", 3, KindReview); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(ctx, "repo", "pr:5", "a", "omp", 3, KindReview); err == nil {
		t.Fatal("two reviews of the same PR would post duplicate comments")
	}
	if _, err := s.CreateRun(ctx, "repo", "#5", "a", "omp", 3, KindIssue); err != nil {
		t.Fatalf("issue #5 is not PR 5: %v", err)
	}
}

func TestCursorShouldReturnNotFoundBeforeFirstSave(t *testing.T) {
	s := openTest(t)
	if _, err := s.Cursor(context.Background(), "repo", 4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a PR never reviewed has no cursor, got %v", err)
	}
}

func TestSaveCursorShouldUpsert(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seen := time.Date(2026, 3, 4, 10, 30, 0, 0, time.FixedZone("BRT", -3*3600))
	first := ReviewCursor{Repo: "repo", PR: 4, LastSeenAt: seen, Rounds: 1, LastHeadSHA: "abc123"}
	if err := s.SaveCursor(ctx, first); err != nil {
		t.Fatal(err)
	}
	other := ReviewCursor{Repo: "repo", PR: 5, LastSeenAt: seen, Rounds: 1, LastHeadSHA: "fff000"}
	if err := s.SaveCursor(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := s.Cursor(ctx, "repo", 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != "repo" || got.PR != 4 || got.Rounds != 1 || got.LastHeadSHA != "abc123" || !got.LastSeenAt.Equal(seen) {
		t.Fatalf("cursor = %+v, want %+v", got, first)
	}

	later := seen.Add(2 * time.Hour)
	if err := s.SaveCursor(ctx, ReviewCursor{Repo: "repo", PR: 4, LastSeenAt: later, Rounds: 2, LastHeadSHA: "def456"}); err != nil {
		t.Fatalf("saving the same PR again must overwrite: %v", err)
	}
	got, _ = s.Cursor(ctx, "repo", 4)
	if got.Rounds != 2 || got.LastHeadSHA != "def456" || !got.LastSeenAt.Equal(later) {
		t.Fatalf("cursor after second save = %+v", got)
	}
	if untouched, _ := s.Cursor(ctx, "repo", 5); untouched.Rounds != 1 || untouched.LastHeadSHA != "fff000" {
		t.Fatalf("another PR's cursor must not change: %+v", untouched)
	}
}
