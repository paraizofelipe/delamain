package store

import (
	"context"
	"database/sql"
	"time"
)

// AgentSession is one omp session of a role in a run: a role that runs again in a later
// iteration gets a new session instead of replacing the earlier one.
type AgentSession struct {
	ID, RunID int64
	Role      string
	Iteration int
	Model     string
	State     AgentState
	StartedAt time.Time
	EndedAt   time.Time // zero while running
	Usage     Usage
}

// StartSession records a running session and returns its id.
func (s *Store) StartSession(ctx context.Context, a AgentSession) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_sessions (run_id, role, iteration, model, state, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
		a.RunID, a.Role, a.Iteration, a.Model, AgentRunning, s.now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndSession marks session id as finished with state.
func (s *Store) EndSession(ctx context.Context, id int64, state AgentState) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_sessions SET state=?, ended_at=? WHERE id=?`, state, s.now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Sessions lists the sessions of runIDs in start order, keyed by run. Runs without sessions are absent.
// A session's Usage is the ledger of its role from its start to the start of the role's next
// session, the last one keeping the rest: the meter bills a session after it ends.
func (s *Store) Sessions(ctx context.Context, runIDs []int64) (map[int64][]AgentSession, error) {
	out := map[int64][]AgentSession{}
	for _, id := range runIDs {
		ss, err := s.runSessions(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(ss) == 0 {
			continue
		}
		if err := s.splitLedger(ctx, id, ss); err != nil {
			return nil, err
		}
		out[id] = ss
	}
	return out, nil
}

// splitLedger adds each ledger row of run to the last session of its role started by then.
// The window is compared on time.Time, not on the text the driver stores.
func (s *Store) splitLedger(ctx context.Context, runID int64, ss []AgentSession) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, at, input, output, cache_read, cache_write, cost_usd FROM token_ledger WHERE run_id = ?`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		var at time.Time
		var u Usage
		if err := rows.Scan(&role, &at, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			return err
		}
		for i := len(ss) - 1; i >= 0; i-- {
			if ss[i].Role == role && !ss[i].StartedAt.After(at) {
				ss[i].Usage = ss[i].Usage.Add(u)
				break
			}
		}
	}
	return rows.Err()
}

func (s *Store) runSessions(ctx context.Context, runID int64) ([]AgentSession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, run_id, role, iteration, model, state, started_at, ended_at FROM agent_sessions WHERE run_id = ? ORDER BY started_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentSession
	for rows.Next() {
		var a AgentSession
		var ended sql.NullTime
		if err := rows.Scan(&a.ID, &a.RunID, &a.Role, &a.Iteration, &a.Model, &a.State, &a.StartedAt, &ended); err != nil {
			return nil, err
		}
		a.EndedAt = ended.Time
		out = append(out, a)
	}
	return out, rows.Err()
}
