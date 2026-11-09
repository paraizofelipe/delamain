package api

import (
	"path/filepath"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/control"
)

// Repo is a repository the API may start or queue.
type Repo struct {
	Path  string    `json:"path"`
	Name  string    `json:"name"`
	Forge *ForgeRef `json:"forge,omitempty"`
}

// ForgeRef is where a repository lives on its forge.
type ForgeRef struct {
	Kind string `json:"kind"` // github | gitlab
	Host string `json:"host"`
	Path string `json:"path"` // owner/repo
}

// Status is the state of the machine: the daemon, the slots and the day's spend.
type Status struct {
	Daemon       bool        `json:"daemon"`
	SlotsInUse   int         `json:"slots_in_use"`
	SlotsMax     int         `json:"slots_max"`
	QueueRunning bool        `json:"queue_running"`
	Today        board.Spend `json:"today"`
	TodayLevel   string      `json:"today_level"` // ok | soft | hard
	Version      string      `json:"version"`
	ServerTime   time.Time   `json:"server_time,omitzero"`
}

// Owner is the process driving a run.
type Owner struct {
	PID   int  `json:"pid"`
	Alive bool `json:"alive"`
}

// RunSummary is a run as the lists and the event stream carry it.
type RunSummary struct {
	ID              int64         `json:"id"`
	Kind            string        `json:"kind"` // issue | review | address
	Repo            string        `json:"repo"` // the full path
	RepoName        string        `json:"repo_name"`
	Ref             string        `json:"ref"` // "#12", "pr:7" or "task:<slug>"
	Title           string        `json:"title"`
	State           string        `json:"state"`
	Column          string        `json:"column"` // where the board puts it
	Outcome         string        `json:"outcome,omitempty"`
	Reason          string        `json:"reason,omitempty"`
	Iteration       int           `json:"iteration"`
	MaxIterations   int           `json:"max_iterations"`
	Complexity      string        `json:"complexity,omitempty"`
	Branch          string        `json:"branch,omitempty"`
	PR              string        `json:"pr,omitempty"`
	Live            bool          `json:"live"`
	Orphaned        bool          `json:"orphaned"`
	Owner           Owner         `json:"owner"`
	StopRequestedAt *time.Time    `json:"stop_requested_at,omitempty"`
	Spend           board.Spend   `json:"spend"`
	Rounds          []board.Round `json:"rounds"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// RunDetail is a run with its artifacts: the plan, each iteration's implementation, gates and
// review, the PR review or the responses, the report and the agents.
type RunDetail struct {
	RunSummary
	RunDir        string                  `json:"run_dir"`
	Plan          any                     `json:"plan,omitempty"`
	PlanError     string                  `json:"plan_error,omitempty"`
	Iterations    []board.IterationDetail `json:"iterations"`
	PRReview      any                     `json:"pr_review,omitempty"`
	Responses     any                     `json:"responses,omitempty"`
	ArtifactError string                  `json:"artifact_error,omitempty"`
	Report        string                  `json:"report,omitempty"`
	Agents        []board.Agent           `json:"agents"`
}

// QueueItem is one issue waiting in the queue, addressed by its id.
type QueueItem struct {
	ID       int64     `json:"id"`
	Position int       `json:"position"`
	Repo     string    `json:"repo"`
	RepoName string    `json:"repo_name"`
	Issue    int       `json:"issue"`
	Title    string    `json:"title"`
	AddedAt  time.Time `json:"added_at"`
	Waiting  string    `json:"waiting,omitempty"`
}

// LogLine is one line of a run's logs.
type LogLine struct {
	TS     string `json:"ts"` // HH:MM:SS, or "" before the first stamp
	Source string `json:"source"`
	Text   string `json:"text"`
}

// Operation is an asynchronous action, as control follows it.
type Operation struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`   // start_run | start_review | recover
	Status    string     `json:"status"` // running | succeeded | failed
	Repo      string     `json:"repo,omitempty"`
	Issue     int        `json:"issue,omitempty"`
	PR        int        `json:"pr,omitempty"`
	RunID     int64      `json:"run_id,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	Result    string     `json:"result,omitempty"`
	LogPath   string     `json:"log_path,omitempty"`
	LogTail   string     `json:"log_tail,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// StopResult is what a stop did.
type StopResult struct {
	RunID     int64  `json:"run_id"`
	Status    string `json:"status"` // requested | recovered | already_finished
	State     string `json:"state"`
	OwnerPID  int    `json:"owner_pid,omitempty"`
	Recovered string `json:"recovered,omitempty"` // how the orphan ended, for recovered
}

// list is a JSON list of items, with the cursor of the next page when there is one.
type list[T any] struct {
	Items      []T    `json:"items"`
	NextBefore *int64 `json:"next_before,omitempty"`
}

func operationOf(op control.Operation) Operation {
	return Operation{
		ID: op.ID, Kind: string(op.Kind), Status: string(op.Status), Repo: op.Repo, Issue: op.Issue, PR: op.PR,
		RunID: op.RunID, ExitCode: op.ExitCode, Error: op.Error, Result: op.Result, LogPath: op.LogPath,
		LogTail: op.LogTail, StartedAt: op.StartedAt, EndedAt: op.EndedAt,
	}
}

// summaryOf converts a board run, with its rounds from b.
func summaryOf(r board.Run, b *board.Builder) RunSummary {
	s := RunSummary{
		ID: r.ID, Kind: r.Kind, Repo: r.Repo, RepoName: filepath.Base(r.Repo), Ref: r.Issue, Title: r.Title,
		State: r.State, Column: board.ColumnOf(r), Iteration: r.Iteration, MaxIterations: r.MaxIterations,
		Complexity: r.Complexity, Branch: r.Branch, PR: r.PR, Orphaned: r.Orphaned,
		Owner: Owner{PID: r.OwnerPID, Alive: r.OwnerPID != 0 && !r.Orphaned}, StopRequestedAt: r.StopRequestedAt,
		Spend: r.Usage, Rounds: []board.Round{}, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if s.Kind == "" {
		s.Kind = "issue"
	}
	terminal := s.Column == board.ColDone || (s.Column == board.ColHuman && !r.Orphaned)
	s.Live = !terminal && !r.Orphaned
	if terminal {
		s.Outcome, s.Reason = board.Outcome(r.Reason), board.ReasonText(r.Reason)
		if s.Outcome == "" {
			s.Outcome = r.State
		}
		s.Owner.Alive = false
	} else if r.Orphaned {
		s.Outcome = "orphaned"
	}
	if b != nil {
		if rounds := b.Rounds(r); rounds != nil {
			s.Rounds = rounds
		}
	}
	return s
}

// detailOf converts what the board builder read of a run.
func detailOf(r board.Run, d board.Detail, b *board.Builder) RunDetail {
	out := RunDetail{
		RunSummary: summaryOf(r, b), RunDir: d.RunDir, PlanError: d.PlanError, Iterations: d.Iterations,
		ArtifactError: d.ArtifactError, Report: d.Report, Agents: d.Agents,
	}
	if d.Plan != nil {
		out.Plan = d.Plan
	}
	if d.PRReview != nil {
		out.PRReview = d.PRReview
	}
	if d.Responses != nil {
		out.Responses = d.Responses
	}
	if out.Iterations == nil {
		out.Iterations = []board.IterationDetail{}
	}
	if out.Agents == nil {
		out.Agents = []board.Agent{}
	}
	return out
}

func queueItemOf(it board.QueueItem) QueueItem {
	return QueueItem{
		ID: it.ID, Position: it.Position, Repo: it.Repo, RepoName: filepath.Base(it.Repo), Issue: it.Issue,
		Title: it.Title, AddedAt: it.AddedAt, Waiting: it.Waiting,
	}
}

func queueOf(items []board.QueueItem) []QueueItem {
	out := make([]QueueItem, 0, len(items))
	for _, it := range items {
		out = append(out, queueItemOf(it))
	}
	return out
}
