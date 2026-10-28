package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func enqueue(t *testing.T, s *Store, issues ...int) {
	t.Helper()
	for _, n := range issues {
		if _, err := s.Enqueue(context.Background(), "repo", n, "title"); err != nil {
			t.Fatal(err)
		}
	}
}

func queued(t *testing.T, s *Store) []int {
	t.Helper()
	items, err := s.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for i, it := range items {
		if it.Position != i+1 {
			t.Fatalf("positions are the rank from 1: %+v", items)
		}
		out = append(out, it.Issue)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEnqueueShouldAppendInOrderFromPositionOne(t *testing.T) {
	s := openTest(t)
	it, err := s.Enqueue(context.Background(), "repo", 12, "fix login")
	if err != nil || it.Position != 1 || it.Issue != 12 || it.Title != "fix login" {
		t.Fatalf("first item: %+v %v", it, err)
	}
	enqueue(t, s, 21, 30)
	if got := queued(t, s); !equalInts(got, []int{12, 21, 30}) {
		t.Fatalf("queue order: %v", got)
	}
}

func TestEnqueueShouldRefuseAnIssueAlreadyQueued(t *testing.T) {
	s := openTest(t)
	enqueue(t, s, 12)
	if _, err := s.Enqueue(context.Background(), "repo", 12, "again"); !errors.Is(err, ErrQueued) {
		t.Fatalf("want ErrQueued, got %v", err)
	}
	if _, err := s.Enqueue(context.Background(), "other", 12, "same number, other repo"); err != nil {
		t.Fatalf("the same number in another repository is another issue: %v", err)
	}
}

func TestRemoveQueueAtShouldRemoveThePositionAndCloseTheGap(t *testing.T) {
	s := openTest(t)
	enqueue(t, s, 12, 21, 30)
	it, err := s.RemoveQueueAt(context.Background(), 2)
	if err != nil || it.Issue != 21 {
		t.Fatalf("removed: %+v %v", it, err)
	}
	if got := queued(t, s); !equalInts(got, []int{12, 30}) {
		t.Fatalf("queue after rm: %v", got)
	}
	for _, p := range []int{0, 3} {
		if _, err := s.RemoveQueueAt(context.Background(), p); !errors.Is(err, ErrNotFound) {
			t.Fatalf("position %d is outside the queue: %v", p, err)
		}
	}
}

func TestSwapQueueShouldExchangeOnlyTheTwoPositions(t *testing.T) {
	s := openTest(t)
	var issues []int
	for n := 1; n <= 13; n++ {
		issues = append(issues, 100+n)
	}
	enqueue(t, s, issues...)
	a, b, err := s.SwapQueue(context.Background(), 1, 13)
	if err != nil || a.Issue != 101 || b.Issue != 113 || a.Position != 13 || b.Position != 1 {
		t.Fatalf("swap: %+v %+v %v", a, b, err)
	}
	want := append([]int{113}, issues[1:12]...)
	want = append(want, 101)
	if got := queued(t, s); !equalInts(got, want) {
		t.Fatalf("queue after swap: %v, want %v", got, want)
	}
	if _, _, err := s.SwapQueue(context.Background(), 1, 14); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a position outside the queue is refused: %v", err)
	}
}

func TestDequeueShouldReportWhetherTheItemWasStillQueued(t *testing.T) {
	s := openTest(t)
	it, err := s.Enqueue(context.Background(), "repo", 12, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Dequeue(context.Background(), it.ID); !ok || err != nil {
		t.Fatalf("first dequeue: %v %v", ok, err)
	}
	if ok, err := s.Dequeue(context.Background(), it.ID); ok || err != nil {
		t.Fatalf("second dequeue finds nothing: %v %v", ok, err)
	}
}

func TestOpenShouldMigrateV9ToTheQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delamain.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	qs := append(append([]string{}, migrations[:9]...), "PRAGMA user_version = 9")
	for _, q := range qs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v9 must migrate: %v", err)
	}
	defer s.Close()
	enqueue(t, s, 7)
	if got := queued(t, s); !equalInts(got, []int{7}) {
		t.Fatalf("queue after migration: %v", got)
	}
}

func TestMigrationShouldAddWaitingToAnExistingQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delamain.db")
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	qs := append(append([]string{}, migrations[:10]...), "PRAGMA user_version = 10",
		`INSERT INTO queue (repo, issue, title, pos, added_at) VALUES ('repo', 21, 'second', 2, '2026-01-01 00:00:00')`,
		`INSERT INTO queue (repo, issue, title, pos, added_at) VALUES ('repo', 12, 'first', 1, '2026-01-01 00:00:00')`)
	for _, q := range qs {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("v10 must migrate: %v", err)
	}
	defer s.Close()
	items, err := s.Queue(ctx)
	if err != nil {
		t.Fatalf("queue after migration: %v", err)
	}
	if len(items) != 2 || items[0].Issue != 12 || items[1].Issue != 21 {
		t.Fatalf("the queued items survive in order: %+v", items)
	}
	for _, it := range items {
		if it.Waiting != "" {
			t.Fatalf("an existing item is not waiting: %+v", it)
		}
	}
}

func waiting(t *testing.T, s *Store) []string {
	t.Helper()
	items, err := s.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Waiting)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSetQueueWaitingShouldPersistTheReason(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	it, err := s.Enqueue(ctx, "repo", 12, "fix login")
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, 21)
	if err := s.SetQueueWaiting(ctx, it.ID, "blocked by #7"); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, s); !equalStrings(got, []string{"blocked by #7", ""}) {
		t.Fatalf("waiting reasons: %q", got)
	}
}

func TestSetQueueWaitingShouldClearTheReasonWithEmpty(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	enqueue(t, s, 12, 21)
	items, err := s.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if err := s.SetQueueWaiting(ctx, it.ID, "blocked"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetQueueWaiting(ctx, items[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := waiting(t, s); !equalStrings(got, []string{"", "blocked"}) {
		t.Fatalf("waiting reasons: %q", got)
	}
}

func TestSetQueueWaitingShouldIgnoreAMissingItem(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	it, err := s.Enqueue(ctx, "repo", 12, "fix login")
	if err != nil {
		t.Fatal(err)
	}
	enqueue(t, s, 21)
	if ok, err := s.Dequeue(ctx, it.ID); !ok || err != nil {
		t.Fatalf("dequeue: %v %v", ok, err)
	}
	if err := s.SetQueueWaiting(ctx, it.ID, "blocked"); err != nil {
		t.Fatalf("an item no longer in the queue is not an error: %v", err)
	}
	if got := queued(t, s); !equalInts(got, []int{21}) {
		t.Fatalf("queue: %v", got)
	}
	if got := waiting(t, s); !equalStrings(got, []string{""}) {
		t.Fatalf("waiting reasons: %q", got)
	}
}

func TestDequeueIssueShouldRemoveOnlyThatIssue(t *testing.T) {
	s := openTest(t)
	enqueue(t, s, 12, 21)
	if err := s.DequeueIssue(context.Background(), "repo", 12); err != nil {
		t.Fatal(err)
	}
	if err := s.DequeueIssue(context.Background(), "repo", 99); err != nil {
		t.Fatalf("an issue not in the queue is not an error: %v", err)
	}
	if got := queued(t, s); !equalInts(got, []int{21}) {
		t.Fatalf("queue: %v", got)
	}
}
