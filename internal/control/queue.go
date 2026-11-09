package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/paraizo/delamain/internal/store"
)

// Why an issue cannot be queued.
var (
	ErrIssueClosed = errors.New("issue is closed")
	ErrNoForge     = errors.New("the repository has no GitHub or GitLab origin")
)

// IssueError is an action refused on one issue.
type IssueError struct {
	Issue int
	Err   error
}

func (e *IssueError) Unwrap() error { return e.Err }

func (e *IssueError) Error() string {
	if errors.Is(e.Err, ErrIssueClosed) {
		return fmt.Sprintf("issue #%d is closed", e.Issue)
	}
	return fmt.Sprintf("issue #%d: %v", e.Issue, e.Err)
}

// Queue lists the queue in execution order.
func (a *Actions) Queue(ctx context.Context) ([]store.QueueItem, error) {
	return a.d.Store.Queue(ctx)
}

// Enqueue appends issue of repo to the queue, with the title the forge gives it, and reports
// whether it was added: an issue already queued comes back as it is, not added. A closed issue,
// one with an active run, a repository without forge or outside AllowedRepos is refused.
func (a *Actions) Enqueue(ctx context.Context, repo string, issue int) (store.QueueItem, bool, error) {
	if issue <= 0 {
		return store.QueueItem{}, false, fmt.Errorf("%w: issue must be a positive number", ErrInvalid)
	}
	repo, err := a.checkRepo(repo)
	if err != nil {
		return store.QueueItem{}, false, err
	}
	top, err := a.d.Git.MainWorktree(ctx, repo)
	if err != nil {
		return store.QueueItem{}, false, fmt.Errorf("repository: %w", err)
	}
	if a.d.ForgeFor == nil {
		return store.QueueItem{}, false, ErrNoForge
	}
	fg, err := a.d.ForgeFor(ctx, top)
	if err != nil {
		return store.QueueItem{}, false, err
	}
	if fg == nil {
		return store.QueueItem{}, false, ErrNoForge
	}
	is, err := fg.Issue(ctx, issue)
	if err != nil {
		return store.QueueItem{}, false, &IssueError{Issue: issue, Err: err}
	}
	if !is.Open {
		return store.QueueItem{}, false, &IssueError{Issue: issue, Err: ErrIssueClosed}
	}
	if err := a.noActiveRun(ctx, top, fmt.Sprintf("#%d", issue)); err != nil {
		return store.QueueItem{}, false, err
	}
	it, err := a.d.Store.Enqueue(ctx, top, issue, is.Title)
	if errors.Is(err, store.ErrQueued) {
		items, err := a.d.Store.Queue(ctx)
		if err != nil {
			return store.QueueItem{}, false, err
		}
		for _, it := range items {
			if it.Repo == top && it.Issue == issue {
				return it, false, nil
			}
		}
		return store.QueueItem{}, false, &IssueError{Issue: issue, Err: ErrNotFound}
	}
	if err != nil {
		return store.QueueItem{}, false, err
	}
	return it, true, nil
}

// RemoveQueueItem takes item id out of the queue.
func (a *Actions) RemoveQueueItem(ctx context.Context, id int64) error {
	ok, err := a.d.Store.Dequeue(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("queue item %d: %w", id, ErrNotFound)
	}
	return nil
}

// MoveQueueItem puts item id at position (from 1; past the end means last) and returns the
// whole queue as it stands after the move.
func (a *Actions) MoveQueueItem(ctx context.Context, id int64, position int) ([]store.QueueItem, error) {
	if err := a.d.Store.MoveQueue(ctx, id, position); errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("queue item %d: %w", id, ErrNotFound)
	} else if err != nil {
		return nil, err
	}
	return a.d.Store.Queue(ctx)
}
