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

// Sessions lists the sessions of runIDs in start order, keyed by run. Runs without sessions are absent.
func (s *Store) Sessions(ctx context.Context, runIDs []int64) (map[int64][]AgentSession, error) {
	out := map[int64][]AgentSession{}
	for _, id := range runIDs {
		ss, err := s.runSessions(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(ss) > 0 {
			out[id] = ss
		}
	}
	return out, nil
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
