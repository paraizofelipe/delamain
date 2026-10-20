// Package store persists runs and the token ledger in SQLite (pure Go driver, no CGO).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// RunState is the pipeline state persisted for resume.
type RunState string

const (
	StateClaimed      RunState = "claimed"
	StatePreflight    RunState = "preflight"
	StateNeedsInfo    RunState = "needs_info"
	StateSetup        RunState = "setup"
	StatePlan         RunState = "plan"
	StateImplement    RunState = "implement"
	StateVerify       RunState = "verify"
	StateReview       RunState = "review"
	StateDeliver      RunState = "deliver"
	StateDeliverDraft RunState = "deliver_draft"
	StateHalted       RunState = "halted"
	StateRecorded     RunState = "recorded"
)

// Terminal reports whether no further pipeline work happens in this state.
func (s RunState) Terminal() bool {
	return s == StateRecorded || s == StateNeedsInfo
}

// RunKind is what a run works on: an issue, a PR review, or addressing a PR's review comments.
type RunKind string

const (
	KindIssue   RunKind = "issue"
	KindReview  RunKind = "review"
	KindAddress RunKind = "address"
)

type Run struct {
	ID        int64
	Repo      string
	IssueRef  string
	Title     string
	Branch    string
	Worktree  string
	State     RunState
	Iteration int
	MRURL     string
	Reason    string
	// OwnerPID is the delamain process driving the run; recovery only touches runs whose owner died.
	OwnerPID  int
	Base      string
	Workspace string
	// Harness is fixed when the run is created; runs older than v4 have "". MaxIterations, PerIssue and
	// Complexity may change once, after planning classifies the issue; runs older than that have 0, 0 and "".
	Harness       string
	MaxIterations int
	PerIssue      float64
	Complexity    string
	// Kind is fixed when the run is created; runs older than v6 are KindIssue.
	Kind      RunKind
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Usage is a cumulative token/cost snapshot of one omp session file.
type Usage struct {
	Input, Output, CacheRead, CacheWrite int64
	CostUSD                              float64
}

func (u Usage) sub(o Usage) Usage {
	return Usage{u.Input - o.Input, u.Output - o.Output, u.CacheRead - o.CacheRead, u.CacheWrite - o.CacheWrite, u.CostUSD - o.CostUSD}
}

func (u Usage) Add(o Usage) Usage {
	return Usage{u.Input + o.Input, u.Output + o.Output, u.CacheRead + o.CacheRead, u.CacheWrite + o.CacheWrite, u.CostUSD + o.CostUSD}
}

// Beyond is how much u exceeds o in each field, never negative.
func (u Usage) Beyond(o Usage) Usage {
	return Usage{max(u.Input-o.Input, 0), max(u.Output-o.Output, 0), max(u.CacheRead-o.CacheRead, 0), max(u.CacheWrite-o.CacheWrite, 0), max(u.CostUSD-o.CostUSD, 0)}
}

func (u Usage) negative() bool {
	return u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 || u.CostUSD < 0
}

func (u Usage) zero() bool { return u == Usage{} }

// BudgetTokens is the budgeted token count: cacheRead is excluded, matching omp's /goal budget.
func (u Usage) BudgetTokens() int64 { return u.Input + u.Output + u.CacheWrite }

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open creates the parent directory and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialize writers; delta computation relies on it
	s := &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`CREATE TABLE runs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		repo       TEXT NOT NULL,
		issue_ref  TEXT NOT NULL,
		title      TEXT NOT NULL DEFAULT '',
		branch     TEXT NOT NULL DEFAULT '',
		worktree   TEXT NOT NULL DEFAULT '',
		state      TEXT NOT NULL,
		iteration  INTEGER NOT NULL DEFAULT 0,
		mr_url     TEXT NOT NULL DEFAULT '',
		reason     TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	);
	CREATE INDEX runs_issue ON runs(repo, issue_ref);
	CREATE TABLE token_snapshots (
		session_file TEXT PRIMARY KEY,
		run_id       INTEGER NOT NULL REFERENCES runs(id),
		role         TEXT NOT NULL,
		input        INTEGER NOT NULL,
		output       INTEGER NOT NULL,
		cache_read   INTEGER NOT NULL,
		cache_write  INTEGER NOT NULL,
		cost_usd     REAL NOT NULL
	);
	CREATE TABLE token_ledger (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id      INTEGER NOT NULL REFERENCES runs(id),
		role        TEXT NOT NULL,
		at          TIMESTAMP NOT NULL,
		input       INTEGER NOT NULL,
		output      INTEGER NOT NULL,
		cache_read  INTEGER NOT NULL,
		cache_write INTEGER NOT NULL,
		cost_usd    REAL NOT NULL
	);
	CREATE INDEX token_ledger_at ON token_ledger(at);
	CREATE INDEX token_ledger_run ON token_ledger(run_id);`,
	`ALTER TABLE runs ADD COLUMN owner_pid INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE runs ADD COLUMN base TEXT NOT NULL DEFAULT '';
	ALTER TABLE runs ADD COLUMN workspace TEXT NOT NULL DEFAULT '';`,
	`CREATE TABLE agents (
		run_id      INTEGER NOT NULL REFERENCES runs(id),
		role        TEXT NOT NULL,
		pid         INTEGER NOT NULL DEFAULT 0,
		state       TEXT NOT NULL,
		log_path    TEXT NOT NULL DEFAULT '',
		session_dir TEXT NOT NULL DEFAULT '',
		started_at  TIMESTAMP NOT NULL,
		ended_at    TIMESTAMP,
		PRIMARY KEY (run_id, role)
	);`,
	`ALTER TABLE runs ADD COLUMN harness TEXT NOT NULL DEFAULT '';
	ALTER TABLE runs ADD COLUMN max_iterations INTEGER NOT NULL DEFAULT 0;`,
	`ALTER TABLE runs ADD COLUMN complexity TEXT NOT NULL DEFAULT '';
	ALTER TABLE runs ADD COLUMN per_issue REAL NOT NULL DEFAULT 0;`,
	`ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'issue';
	CREATE TABLE review_cursors (
		repo          TEXT NOT NULL,
		pr            INTEGER NOT NULL,
		last_seen_at  TIMESTAMP NOT NULL,
		rounds        INTEGER NOT NULL DEFAULT 0,
		last_head_sha TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (repo, pr)
	);`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// CreateRun inserts a claimed run, refusing a second active run for the same issue.
func (s *Store) CreateRun(ctx context.Context, repo, issueRef, title, harness string, maxIterations int, perIssue float64, kind RunKind) (Run, error) {
	if r, err := s.ActiveRun(ctx, repo, issueRef); err == nil {
		return Run{}, fmt.Errorf("issue %s already has active run %d in state %s", issueRef, r.ID, r.State)
	} else if !errors.Is(err, ErrNotFound) {
		return Run{}, err
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (repo, issue_ref, title, state, owner_pid, harness, max_iterations, per_issue, kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		repo, issueRef, title, StateClaimed, os.Getpid(), harness, maxIterations, perIssue, kind, now, now)
	if err != nil {
		return Run{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetRun(ctx, id)
}

// Limits returns the iteration cap and per-issue budget the run is bound by: its recorded ones, or the
// defaults for runs recorded before complexity sizing, which carry no limits of their own.
func (r Run) Limits(defaultMaxIter int, defaultPerIssue float64) (maxIter int, perIssue float64) {
	maxIter, perIssue = r.MaxIterations, r.PerIssue
	if maxIter == 0 {
		maxIter = defaultMaxIter
	}
	if r.Complexity == "" {
		perIssue = defaultPerIssue
	}
	return maxIter, perIssue
}

const runCols = `id, repo, issue_ref, title, branch, worktree, state, iteration, mr_url, reason, owner_pid, base, workspace, harness, max_iterations, per_issue, complexity, kind, created_at, updated_at`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Repo, &r.IssueRef, &r.Title, &r.Branch, &r.Worktree, &r.State, &r.Iteration, &r.MRURL, &r.Reason, &r.OwnerPID, &r.Base, &r.Workspace, &r.Harness, &r.MaxIterations, &r.PerIssue, &r.Complexity, &r.Kind, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id = ?`, id))
}

// ActiveRun returns the non-terminal run for an issue, or ErrNotFound.
func (s *Store) ActiveRun(ctx context.Context, repo, issueRef string) (Run, error) {
	return scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM runs WHERE repo = ? AND issue_ref = ? AND state NOT IN (?, ?) ORDER BY id DESC LIMIT 1`,
		repo, issueRef, StateRecorded, StateNeedsInfo))
}

// UnfinishedRuns lists non-terminal runs, oldest first, for resume after a crash.
func (s *Store) UnfinishedRuns(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM runs WHERE state NOT IN (?, ?) ORDER BY id`, StateRecorded, StateNeedsInfo)
}

func (s *Store) queryRuns(ctx context.Context, query string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentRuns lists the newest runs first, finished ones included.
func (s *Store) RecentRuns(ctx context.Context, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runCols+` FROM runs ORDER BY id DESC LIMIT ?`, limit)
}

// ClaimRun moves ownership of a run from one process to another atomically; false means someone
// else got there first (two recoverers never finalize the same run).
func (s *Store) ClaimRun(ctx context.Context, id int64, from, to int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET owner_pid=?, updated_at=? WHERE id=? AND owner_pid=?`, to, s.now(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UpdateRun persists mutable fields of r.
func (s *Store) UpdateRun(ctx context.Context, r Run) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET branch=?, worktree=?, state=?, iteration=?, mr_url=?, reason=?, base=?, workspace=?, max_iterations=?, per_issue=?, complexity=?, updated_at=? WHERE id=?`,
		r.Branch, r.Worktree, r.State, r.Iteration, r.MRURL, r.Reason, r.Base, r.Workspace, r.MaxIterations, r.PerIssue, r.Complexity, s.now(), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordUsage stores the cumulative snapshot of a session file and appends the delta to the ledger.
// It returns the delta. A shrinking snapshot (rewritten file) resets the baseline without negative entries.
func (s *Store) RecordUsage(ctx context.Context, runID int64, role, sessionFile string, cur Usage) (Usage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Usage{}, err
	}
	defer tx.Rollback()

	var prev Usage
	err = tx.QueryRowContext(ctx,
		`SELECT input, output, cache_read, cache_write, cost_usd FROM token_snapshots WHERE session_file = ?`, sessionFile).
		Scan(&prev.Input, &prev.Output, &prev.CacheRead, &prev.CacheWrite, &prev.CostUSD)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Usage{}, err
	}
	delta := cur.sub(prev)
	if delta.negative() {
		delta = Usage{}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO token_snapshots (session_file, run_id, role, input, output, cache_read, cache_write, cost_usd)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_file) DO UPDATE SET input=excluded.input, output=excluded.output,
		   cache_read=excluded.cache_read, cache_write=excluded.cache_write, cost_usd=excluded.cost_usd`,
		sessionFile, runID, role, cur.Input, cur.Output, cur.CacheRead, cur.CacheWrite, cur.CostUSD); err != nil {
		return Usage{}, err
	}
	if !delta.zero() {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO token_ledger (run_id, role, at, input, output, cache_read, cache_write, cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, role, s.now(), delta.Input, delta.Output, delta.CacheRead, delta.CacheWrite, delta.CostUSD); err != nil {
			return Usage{}, err
		}
	}
	return delta, tx.Commit()
}

func (s *Store) sumLedger(ctx context.Context, where string, args ...any) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(input),0), COALESCE(SUM(output),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0), COALESCE(SUM(cost_usd),0)
		 FROM token_ledger WHERE `+where, args...).
		Scan(&u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.CostUSD)
	return u, err
}

// RunUsage is the total consumed by one run.
func (s *Store) RunUsage(ctx context.Context, runID int64) (Usage, error) {
	return s.sumLedger(ctx, "run_id = ?", runID)
}

// UsageSince is the total consumed by all runs since t (e.g. start of the UTC day).
func (s *Store) UsageSince(ctx context.Context, t time.Time) (Usage, error) {
	return s.sumLedger(ctx, "at >= ?", t.UTC())
}

// ReviewCursor is how far the reviewer got on a PR.
type ReviewCursor struct {
	Repo        string
	PR          int
	LastSeenAt  time.Time
	Rounds      int
	LastHeadSHA string
}

// Cursor returns the review cursor of a PR, or ErrNotFound before the first SaveCursor.
func (s *Store) Cursor(ctx context.Context, repo string, pr int) (ReviewCursor, error) {
	c := ReviewCursor{Repo: repo, PR: pr}
	err := s.db.QueryRowContext(ctx,
		`SELECT last_seen_at, rounds, last_head_sha FROM review_cursors WHERE repo = ? AND pr = ?`, repo, pr).
		Scan(&c.LastSeenAt, &c.Rounds, &c.LastHeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewCursor{}, ErrNotFound
	}
	return c, err
}

// SaveCursor inserts or overwrites the review cursor of c.Repo and c.PR.
func (s *Store) SaveCursor(ctx context.Context, c ReviewCursor) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO review_cursors (repo, pr, last_seen_at, rounds, last_head_sha) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo, pr) DO UPDATE SET last_seen_at=excluded.last_seen_at, rounds=excluded.rounds, last_head_sha=excluded.last_head_sha`,
		c.Repo, c.PR, c.LastSeenAt.UTC(), c.Rounds, c.LastHeadSHA)
	return err
}
