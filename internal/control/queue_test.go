package control

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/store"
)

// issuesForge knows the open issues of open; any other number is closed, unless errs fails it.
type issuesForge struct {
	forge.Forge
	open map[int]string
	errs map[int]error
}

func (f issuesForge) Issue(_ context.Context, n int) (forge.Issue, error) {
	if err := f.errs[n]; err != nil {
		return forge.Issue{}, err
	}
	title, ok := f.open[n]
	return forge.Issue{Number: n, Title: title, Open: ok}, nil
}

// queueActions are the fixture's actions on a forge that knows open, with the repo allowed.
func (f *fixture) queueActions(open map[int]string, errs map[int]error) *Actions {
	f.cfg.Daemon.Repos = []string{f.repo}
	return New(Deps{Cfg: f.cfg, Store: f.st, ForgeFor: func(context.Context, string) (forge.Forge, error) {
		return issuesForge{open: open, errs: errs}, nil
	}})
}

func TestEnqueueShouldAppendOpenIssuesWithTheirTitleAndReportADuplicate(t *testing.T) {
	f := newFixture(t)
	acts := f.queueActions(map[int]string{12: "Fix login", 21: "Add export"}, nil)
	ctx := context.Background()
	it, added, err := acts.Enqueue(ctx, f.repo+"/", 12)
	if err != nil || !added || it.Issue != 12 || it.Title != "Fix login" || it.Position != 1 || it.Repo != f.repo {
		t.Fatalf("the issue is queued with its title at the end: %+v %v %v", it, added, err)
	}
	if it, added, err := acts.Enqueue(ctx, f.repo, 21); err != nil || !added || it.Position != 2 {
		t.Fatalf("the next one goes behind: %+v %v %v", it, added, err)
	}
	again, added, err := acts.Enqueue(ctx, f.repo, 12)
	if err != nil || added || again.ID != it.ID || again.Position != 1 {
		t.Fatalf("an issue already queued comes back as it is, not added: %+v %v %v", again, added, err)
	}
	if items, err := acts.Queue(ctx); err != nil || len(items) != 2 {
		t.Fatalf("queue = %+v %v", items, err)
	}
}

func TestEnqueueShouldRefuseClosedUnknownActiveAndDisallowed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	acts := f.queueActions(map[int]string{12: "a"}, map[int]error{50: errors.New("boom")})
	err := enqueueErr(ctx, acts, f.repo, 40)
	if !errors.Is(err, ErrIssueClosed) || err.Error() != "issue #40 is closed" {
		t.Fatalf("a closed issue: %v", err)
	}
	if err := enqueueErr(ctx, acts, f.repo, 50); err == nil || !strings.Contains(err.Error(), "issue #50: boom") {
		t.Fatalf("a forge error names the issue: %v", err)
	}
	if err := enqueueErr(ctx, acts, f.repo, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("issue 0: %v", err)
	}
	if err := enqueueErr(ctx, acts, "/elsewhere", 12); !errors.Is(err, ErrRepoNotAllowed) {
		t.Fatalf("a repo outside the allowlist: %v", err)
	}
	rec := f.run(t, "#12", "", store.StateImplement, store.KindIssue)
	err = enqueueErr(ctx, acts, f.repo, 12)
	var re *RunError
	if !errors.Is(err, ErrRunActive) || !errors.As(err, &re) || re.ID != rec.ID || err.Error() != "#12 already has active run 1 (implement)" {
		t.Fatalf("an issue with an active run: %v", err)
	}
	if items, _ := acts.Queue(ctx); len(items) != 0 {
		t.Fatalf("nothing was queued: %+v", items)
	}

	noForge := New(Deps{Cfg: f.cfg, Store: f.st, AnyRepo: true, ForgeFor: func(context.Context, string) (forge.Forge, error) { return nil, nil }})
	if err := enqueueErr(ctx, noForge, f.repo, 12); !errors.Is(err, ErrNoForge) {
		t.Fatalf("a repository without forge: %v", err)
	}
	if err := enqueueErr(ctx, New(Deps{Cfg: f.cfg, Store: f.st, AnyRepo: true}), f.repo, 12); !errors.Is(err, ErrNoForge) {
		t.Fatalf("no ForgeFor at all: %v", err)
	}
}

// enqueueErr is Enqueue without the item, for the refusals.
func enqueueErr(ctx context.Context, acts *Actions, repo string, issue int) error {
	_, _, err := acts.Enqueue(ctx, repo, issue)
	return err
}

func TestAnyRepoShouldTrustTheRepositoryNamed(t *testing.T) {
	f := newFixture(t)
	acts := New(Deps{Cfg: f.cfg, Store: f.st, AnyRepo: true, ForgeFor: func(context.Context, string) (forge.Forge, error) {
		return issuesForge{open: map[int]string{1: "x"}}, nil
	}})
	if _, added, err := acts.Enqueue(context.Background(), f.repo, 1); err != nil || !added {
		t.Fatalf("the CLI's --repo needs no allowlist: %v %v", added, err)
	}
}

func TestRemoveAndMoveQueueItemsShouldWorkByID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	acts := f.queueActions(map[int]string{1: "a", 2: "b", 3: "c"}, nil)
	var ids []int64
	for _, n := range []int{1, 2, 3} {
		it, _, err := acts.Enqueue(ctx, f.repo, n)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	items, err := acts.MoveQueueItem(ctx, ids[2], 1)
	if err != nil || len(items) != 3 || items[0].Issue != 3 || items[0].Position != 1 || items[2].Issue != 2 {
		t.Fatalf("the moved queue comes back: %+v %v", items, err)
	}
	if _, err := acts.MoveQueueItem(ctx, 999, 1); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "queue item 999") {
		t.Fatalf("an unknown item: %v", err)
	}
	if err := acts.RemoveQueueItem(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := acts.RemoveQueueItem(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
	if items, _ := acts.Queue(ctx); len(items) != 2 || items[0].Issue != 3 || items[1].Issue != 2 || items[1].Position != 2 {
		t.Fatalf("queue = %+v", items)
	}
}
