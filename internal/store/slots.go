package store

import (
	"context"
	"os"
)

// Slot holders. The queue runs one issue at a time in a slot of its own; while it has issues,
// or one of them runs, the others (delamain run, the daemon's labeled issues and reviews) share
// the remaining slots.
const (
	HolderQueue  = "queue"
	HolderRun    = "run"
	HolderDaemon = "daemon"
)

// Every slot statement below is a single statement: SQLite serializes writers, so the counts it
// checks cannot change before it writes, whatever process runs it. The held column of the table
// is always 1.

// queueReserved is true while the queue keeps a slot: it has issues, or one of them runs (the
// running one already left the queue table).
const queueReserved = `(EXISTS (SELECT 1 FROM queue) OR EXISTS (SELECT 1 FROM slots WHERE holder = 'queue'))`

// PruneSlots frees the slots of the processes that died.
func (s *Store) PruneSlots(ctx context.Context, alive func(pid int) bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT pid FROM slots`)
	if err != nil {
		return err
	}
	var dead []int
	for rows.Next() {
		var pid int
		if err := rows.Scan(&pid); err != nil {
			rows.Close()
			return err
		}
		if !alive(pid) {
			dead = append(dead, pid)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, pid := range dead {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM slots WHERE pid = ?`, pid); err != nil {
			return err
		}
	}
	return nil
}

// AcquireSlot takes one of the max slots for holder; false means none is free for it. The queue
// gets one slot at most, and the other holders leave it one while it is reserved.
func (s *Store) AcquireSlot(ctx context.Context, holder string, maxSlots int) (int64, bool, error) {
	cond := `(SELECT count(*) FROM slots) < ?4 AND
		(SELECT count(*) FROM slots WHERE holder != 'queue') < ?4 - (CASE WHEN ` + queueReserved + ` THEN 1 ELSE 0 END)`
	if holder == HolderQueue {
		cond = `(SELECT count(*) FROM slots) < ?4 AND NOT EXISTS (SELECT 1 FROM slots WHERE holder = 'queue')`
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO slots (pid, holder, held, created_at) SELECT ?1, ?2, 1, ?3 WHERE `+cond,
		os.Getpid(), holder, s.now(), maxSlots)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	return id, err == nil, err
}

// ReleaseSlot frees slot id.
func (s *Store) ReleaseSlot(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM slots WHERE id = ?`, id)
	return err
}

// SlotUsage is how the slots stand: how many are in use, whether an issue of the queue runs in
// one, and whether one is reserved for the queue.
type SlotUsage struct {
	InUse        int
	QueueRunning bool
	Reserved     bool
}

// SlotUsage counts the slots of the processes alive reports alive (nil counts them all), so a
// reader that does not prune, as `watch`, leaves out the slots of the processes that died.
func (s *Store) SlotUsage(ctx context.Context, alive func(pid int) bool) (SlotUsage, error) {
	var u SlotUsage
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM queue)`).Scan(&u.Reserved); err != nil {
		return u, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT pid, holder FROM slots`)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid int
		var holder string
		if err := rows.Scan(&pid, &holder); err != nil {
			return u, err
		}
		if alive != nil && !alive(pid) {
			continue
		}
		u.InUse++
		if holder == HolderQueue {
			u.QueueRunning, u.Reserved = true, true
		}
	}
	return u, rows.Err()
}
