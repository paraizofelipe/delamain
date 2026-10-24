package store

import (
	"context"
	"os"
)

// Slot holders. A run waits for a slot and takes it ahead of the daemon; the daemon only gets
// one when no run is waiting.
const (
	HolderRun    = "run"
	HolderDaemon = "daemon"
)

// Every slot statement below is a single statement: SQLite serializes writers, so the count it
// checks cannot change before it writes, whatever process runs it.

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

// WaitSlot records a run of this process waiting for a slot; TakeSlot turns it into one.
func (s *Store) WaitSlot(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO slots (pid, holder, held, created_at) VALUES (?, ?, 0, ?)`,
		os.Getpid(), HolderRun, s.now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TakeSlot gives the waiting slot id one of the max slots; false means they are all held.
func (s *Store) TakeSlot(ctx context.Context, id int64, maxSlots int) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE slots SET held = 1 WHERE id = ? AND held = 0 AND (SELECT count(*) FROM slots WHERE held = 1) < ?`, id, maxSlots)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AcquireDaemonSlot takes one of the max slots for the daemon, unless they are all held or a
// run is waiting for one: runs go ahead of the queue.
func (s *Store) AcquireDaemonSlot(ctx context.Context, maxSlots int) (int64, bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO slots (pid, holder, held, created_at) SELECT ?, ?, 1, ?
		 WHERE (SELECT count(*) FROM slots WHERE held = 1) < ? AND NOT EXISTS (SELECT 1 FROM slots WHERE held = 0)`,
		os.Getpid(), HolderDaemon, s.now(), maxSlots)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	return id, err == nil, err
}

// ReleaseSlot frees slot id, held or waiting.
func (s *Store) ReleaseSlot(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM slots WHERE id = ?`, id)
	return err
}

// SlotsInUse is how many slots are held.
func (s *Store) SlotsInUse(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM slots WHERE held = 1`).Scan(&n)
	return n, err
}
