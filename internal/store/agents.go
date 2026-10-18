package store

import (
	"context"
	"database/sql"
	"time"
)

// AgentState is the lifecycle of one omp session of a run.
type AgentState string

const (
	AgentRunning AgentState = "running"
	AgentDone    AgentState = "done"
	// AgentFailed is a session that did not stop cleanly, including one whose owner process died.
	AgentFailed AgentState = "failed"
)

// Agent is the record of a role's omp session, so other processes can see what a run is doing.
type Agent struct {
	RunID      int64
	Role       string
	PID        int
	State      AgentState
	LogPath    string
	SessionDir string
	StartedAt  time.Time
	EndedAt    time.Time // zero while running
}

// StartAgent records a running session for a role, replacing an earlier one of the same role.
func (s *Store) StartAgent(ctx context.Context, a Agent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agents (run_id, role, pid, state, log_path, session_dir, started_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(run_id, role) DO UPDATE SET pid=excluded.pid, state=excluded.state, log_path=excluded.log_path,
		   session_dir=excluded.session_dir, started_at=excluded.started_at, ended_at=NULL`,
		a.RunID, a.Role, a.PID, AgentRunning, a.LogPath, a.SessionDir, s.now())
	return err
}

// EndAgent marks a role's session as finished with state.
func (s *Store) EndAgent(ctx context.Context, runID int64, role string, state AgentState) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET state=?, ended_at=? WHERE run_id=? AND role=?`, state, s.now(), runID, role)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FailRunningAgents marks the sessions still running as failed: their owner died with them.
func (s *Store) FailRunningAgents(ctx context.Context, runID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET state=?, ended_at=? WHERE run_id=? AND state=?`, AgentFailed, s.now(), runID, AgentRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Agents lists a run's sessions in start order.
func (s *Store) Agents(ctx context.Context, runID int64) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, role, pid, state, log_path, session_dir, started_at, ended_at FROM agents WHERE run_id = ? ORDER BY started_at, rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var a Agent
		var ended sql.NullTime
		if err := rows.Scan(&a.RunID, &a.Role, &a.PID, &a.State, &a.LogPath, &a.SessionDir, &a.StartedAt, &ended); err != nil {
			return nil, err
		}
		a.EndedAt = ended.Time
		out = append(out, a)
	}
	return out, rows.Err()
}

// RoleUsage is a run's consumption per role, subagents included in their parent's role.
func (s *Store) RoleUsage(ctx context.Context, runID int64) (map[string]Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT role, SUM(input), SUM(output), SUM(cache_read), SUM(cache_write), SUM(cost_usd) FROM token_ledger WHERE run_id = ? GROUP BY role`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Usage{}
	for rows.Next() {
		var role string
		var u Usage
		if err := rows.Scan(&role, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD); err != nil {
			return nil, err
		}
		out[role] = u
	}
	return out, rows.Err()
}
