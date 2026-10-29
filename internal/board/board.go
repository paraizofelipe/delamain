// Package board serves `delamain board`: a read-only kanban of the runs in the browser, with one
// column per stage of the pipeline, that a Server-Sent Events stream keeps up to date.
//
// The runs are driven by other processes (the daemon, `delamain run`), so the board learns what
// changed the way `watch` does: it reloads the state database and the artifacts of the runs every
// interval, and pushes a snapshot to the browsers only when it differs from the last one.
package board

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is what Source.Run returns for an ID that names no run.
var ErrNotFound = errors.New("run not found")

// Source is everything the board reads; it has no method that changes a run.
type Source interface {
	// Runs returns the active runs and up to finished ended ones.
	Runs(ctx context.Context, finished int) ([]Run, error)
	// Run returns one run, or an error wrapping ErrNotFound.
	Run(ctx context.Context, id int64) (Run, error)
	// Queue lists the queue in execution order.
	Queue(ctx context.Context) ([]QueueItem, error)
	Status(ctx context.Context) (Status, error)
	// Report returns the Relatório page for q, already parsed by ParseReportQuery.
	Report(ctx context.Context, q ReportQuery) (Report, error)
}

// Run is a run as the board needs it.
type Run struct {
	ID            int64
	Kind          string // issue | review | address
	Repo          string
	Issue         string
	Title         string
	State         string
	Reason        string // "<outcome>: <why>" once recorded
	Iteration     int
	MaxIterations int
	Complexity    string
	Branch        string
	PR            string
	// Orphaned is an active run whose owner died: it waits for `delamain recover`.
	Orphaned  bool
	Spend     string // the run's spend, as `status` shows it
	Dir       string // the run directory, where the artifacts are
	Agents    []Agent
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Agent is one role session of a run.
type Agent struct {
	Role      string     `json:"role"`
	State     string     `json:"state"` // running | done | failed
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Tokens    int64      `json:"tokens"`
	CostUSD   float64    `json:"cost_usd"`
}

// QueueItem is one issue waiting in the queue.
type QueueItem struct {
	Position int       `json:"position"`
	Repo     string    `json:"repo"`
	Issue    int       `json:"issue"`
	Title    string    `json:"title"`
	AddedAt  time.Time `json:"added_at"`
	Waiting  string    `json:"waiting,omitempty"` // what the item waits for before it can start
}

// Status is the header of the board: the daemon, the slots and the day's spend.
type Status struct {
	Daemon       bool   `json:"daemon"`
	SlotsInUse   int    `json:"slots_in_use"`
	SlotsMax     int    `json:"slots_max"`
	QueueRunning bool   `json:"queue_running"`
	Today        string `json:"today"`       // the day's spend
	TodayLimit   string `json:"today_limit"` // the daily budget; empty without one
	TodayLevel   string `json:"today_level"` // ok | soft | hard
}

// Column keys, in the order of the board.
const (
	ColQueue     = "queue"
	ColSetup     = "setup"
	ColPlan      = "plan"
	ColImplement = "implement"
	ColVerify    = "verify"
	ColReview    = "review"
	ColDeliver   = "deliver"
	ColDone      = "done"
	ColHuman     = "human"
)

// Column is one stage of the pipeline and the cards in it.
type Column struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Who   string `json:"who"` // who works the stage
	Cards []Card `json:"cards"`
}

func columns() []Column {
	return []Column{
		{Key: ColQueue, Label: "Fila", Who: "queue · slot reservado"},
		{Key: ColSetup, Label: "Setup", Who: "worktree · gates"},
		{Key: ColPlan, Label: "Plano", Who: "planner · analyzer"},
		{Key: ColImplement, Label: "Implementação", Who: "implementer"},
		{Key: ColVerify, Label: "Verificação", Who: "gates"},
		{Key: ColReview, Label: "Revisão", Who: "spec · standards · security"},
		{Key: ColDeliver, Label: "Entrega", Who: "push · PR/MR"},
		{Key: ColDone, Label: "Concluída", Who: "entregue"},
		{Key: ColHuman, Label: "Precisa de humano", Who: "sua vez"},
	}
}

// Outcomes recorded in Run.Reason that need nothing else from a person.
var settled = map[string]bool{"approved": true, "reviewed": true, "answered": true, "skipped": true}

// outcome is the prefix Runner.record writes to the reason of a recorded run.
func outcome(reason string) string {
	o, _, _ := strings.Cut(reason, ":")
	return strings.TrimSpace(o)
}

// detail is the reason of a recorded run without its outcome.
func detail(reason string) string {
	_, d, found := strings.Cut(reason, ":")
	if !found {
		return ""
	}
	return strings.TrimSpace(d)
}

func terminal(state string) bool {
	return state == "recorded" || state == "needs_info" || state == "halted"
}

// columnOf places a run on the board.
func columnOf(r Run) string {
	if terminal(r.State) {
		if r.State == "recorded" && settled[outcome(r.Reason)] {
			return ColDone
		}
		return ColHuman
	}
	if r.Orphaned {
		return ColHuman
	}
	switch r.State {
	case "plan":
		return ColPlan
	case "implement":
		return ColImplement
	case "verify":
		return ColVerify
	case "review":
		return ColReview
	case "deliver", "deliver_draft":
		return ColDeliver
	}
	return ColSetup
}
