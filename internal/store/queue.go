package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrQueued refuses an issue that is already in the queue.
var ErrQueued = errors.New("already in the queue")

// QueueItem is one issue waiting for the daemon to start it. Position is its rank, from 1: the
// next one to run. Waiting is why it is held back, empty when it is not.
type QueueItem struct {
	ID       int64
	Position int
	Repo     string
	Issue    int
	Title    string
	AddedAt  time.Time
	Waiting  string
}

// Enqueue appends issue of repo to the end of the queue.
func (s *Store) Enqueue(ctx context.Context, repo string, issue int, title string) (QueueItem, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO queue (repo, issue, title, pos, added_at) VALUES (?, ?, ?, (SELECT COALESCE(MAX(pos), 0) + 1 FROM queue), ?)`,
		repo, issue, title, s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return QueueItem{}, fmt.Errorf("issue #%d: %w", issue, ErrQueued)
		}
		return QueueItem{}, err
	}
	items, err := s.Queue(ctx)
	if err != nil {
		return QueueItem{}, err
	}
	for _, it := range items {
		if it.Repo == repo && it.Issue == issue {
			return it, nil
		}
	}
	return QueueItem{}, ErrNotFound // removed by someone else in the meantime
}

// Queue lists the queue in execution order.
func (s *Store) Queue(ctx context.Context) ([]QueueItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo, issue, title, added_at, waiting FROM queue ORDER BY pos, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		it := QueueItem{Position: len(out) + 1}
		if err := rows.Scan(&it.ID, &it.Repo, &it.Issue, &it.Title, &it.AddedAt, &it.Waiting); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// at is the item at position, or ErrNotFound outside the queue.
func (s *Store) at(ctx context.Context, position int) (QueueItem, error) {
	items, err := s.Queue(ctx)
	if err != nil {
		return QueueItem{}, err
	}
	if position < 1 || position > len(items) {
		return QueueItem{}, fmt.Errorf("position %d: %w (the queue has %d)", position, ErrNotFound, len(items))
	}
	return items[position-1], nil
}

// RemoveQueueAt removes the item at position and returns it.
func (s *Store) RemoveQueueAt(ctx context.Context, position int) (QueueItem, error) {
	it, err := s.at(ctx, position)
	if err != nil {
		return QueueItem{}, err
	}
	ok, err := s.Dequeue(ctx, it.ID)
	if err != nil {
		return QueueItem{}, err
	}
	if !ok {
		return QueueItem{}, fmt.Errorf("position %d: %w", position, ErrNotFound)
	}
	return it, nil
}

// SwapQueue exchanges the items at positions a and b and returns them, a's first.
func (s *Store) SwapQueue(ctx context.Context, a, b int) (QueueItem, QueueItem, error) {
	ia, err := s.at(ctx, a)
	if err != nil {
		return QueueItem{}, QueueItem{}, err
	}
	ib, err := s.at(ctx, b)
	if err != nil {
		return QueueItem{}, QueueItem{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return QueueItem{}, QueueItem{}, err
	}
	defer tx.Rollback()
	var pa, pb int
	if err := tx.QueryRowContext(ctx, `SELECT pos FROM queue WHERE id = ?`, ia.ID).Scan(&pa); err != nil {
		return QueueItem{}, QueueItem{}, fmt.Errorf("position %d: %w", a, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT pos FROM queue WHERE id = ?`, ib.ID).Scan(&pb); err != nil {
		return QueueItem{}, QueueItem{}, fmt.Errorf("position %d: %w", b, err)
	}
	for _, u := range [][2]any{{pb, ia.ID}, {pa, ib.ID}} {
		if _, err := tx.ExecContext(ctx, `UPDATE queue SET pos = ? WHERE id = ?`, u[0], u[1]); err != nil {
			return QueueItem{}, QueueItem{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return QueueItem{}, QueueItem{}, err
	}
	ia.Position, ib.Position = b, a
	return ia, ib, nil
}

// Dequeue removes the item id; false means it was no longer in the queue.
func (s *Store) Dequeue(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM queue WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InQueue reports whether the item id is still in the queue.
func (s *Store) InQueue(ctx context.Context, id int64) (bool, error) {
	var in bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM queue WHERE id = ?)`, id).Scan(&in)
	return in, err
}

// DequeueIssue removes issue of repo from the queue, if it is there: it became a run some other
// way (delamain run, the ready label).
func (s *Store) DequeueIssue(ctx context.Context, repo string, issue int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM queue WHERE repo = ? AND issue = ?`, repo, issue)
	return err
}

// SetQueueWaiting records why the item id is held back; an empty reason clears it. An item no
// longer in the queue is not an error.
func (s *Store) SetQueueWaiting(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE queue SET waiting = ? WHERE id = ?`, reason, id)
	return err
}
