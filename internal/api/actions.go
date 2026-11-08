package api

import (
	"net/http"
	"strconv"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/control"
	"github.com/paraizo/delamain/internal/store"
)

// actionRoutes are the routes that change something; every one is audited.
func (s *Server) actionRoutes(mux *router) {
	mux.HandleFunc("POST /api/v1/runs", s.handleStartRun)
	mux.HandleFunc("POST /api/v1/reviews", s.handleStartReview)
	mux.HandleFunc("POST /api/v1/runs/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /api/v1/runs/{id}/recover", s.handleRecover)
	mux.HandleFunc("DELETE /api/v1/runs/{id}", s.handleRemove)
	mux.HandleFunc("POST /api/v1/queue", s.handleEnqueue)
	mux.HandleFunc("DELETE /api/v1/queue/{id}", s.handleDequeue)
	mux.HandleFunc("PUT /api/v1/queue/{id}/position", s.handleMoveQueue)
	mux.HandleFunc("GET /api/v1/operations", s.handleOperations)
	mux.HandleFunc("GET /api/v1/operations/{id}", s.handleOperation)
}

// startRunRequest is the body of POST /runs.
type startRunRequest struct {
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
	Kind  string `json:"kind"` // feature | bug | ""; the run infers it when empty
}

// startReviewRequest is the body of POST /reviews.
type startReviewRequest struct {
	Repo string `json:"repo"`
	PR   int    `json:"pr"`
}

// enqueueRequest is the body of POST /queue.
type enqueueRequest struct {
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
}

// moveRequest is the body of PUT /queue/{id}/position.
type moveRequest struct {
	Position int `json:"position"`
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var body startRunRequest
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, r, err)
		return
	}
	switch body.Kind {
	case "", "feature", "bug":
	default:
		writeProblem(w, r, http.StatusBadRequest, codeInvalidBody, "kind must be feature or bug", nil)
		return
	}
	noteAudit(r, "repo", body.Repo)
	noteAudit(r, "issue", body.Issue)
	op, err := s.acts.StartIssue(r.Context(), body.Repo, body.Issue)
	if err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "operation_id", op.ID)
	writeJSON(w, http.StatusAccepted, operationOf(op))
}

func (s *Server) handleStartReview(w http.ResponseWriter, r *http.Request) {
	var body startReviewRequest
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "repo", body.Repo)
	noteAudit(r, "pr", body.PR)
	op, err := s.acts.StartReview(r.Context(), body.Repo, body.PR)
	if err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "operation_id", op.ID)
	writeJSON(w, http.StatusAccepted, operationOf(op))
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	noteAudit(r, "run_id", id)
	res, err := s.acts.Stop(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := StopResult{RunID: id, Status: string(res.Status), State: string(res.State), OwnerPID: res.OwnerPID}
	if res.Recovered != nil {
		out.Recovered = control.RecoveredResult(*res.Recovered)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	noteAudit(r, "run_id", id)
	op, err := s.acts.RecoverOperation(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "operation_id", op.ID)
	writeJSON(w, http.StatusAccepted, operationOf(op))
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	noteAudit(r, "run_id", id)
	noteAudit(r, "force", force)
	if err := s.acts.Remove(r.Context(), id, force); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	var body enqueueRequest
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "repo", body.Repo)
	noteAudit(r, "issue", body.Issue)
	it, added, err := s.acts.Enqueue(r.Context(), body.Repo, body.Issue)
	if err != nil {
		fail(w, r, err)
		return
	}
	noteAudit(r, "queue_id", it.ID)
	status := http.StatusOK
	if added {
		status = http.StatusCreated
	}
	writeJSON(w, status, queueItemOf(boardQueueItem(it)))
}

func (s *Server) handleDequeue(w http.ResponseWriter, r *http.Request) {
	id, ok := queueID(w, r)
	if !ok {
		return
	}
	noteAudit(r, "queue_id", id)
	if err := s.acts.RemoveQueueItem(r.Context(), id); err != nil {
		fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMoveQueue(w http.ResponseWriter, r *http.Request) {
	id, ok := queueID(w, r)
	if !ok {
		return
	}
	var body moveRequest
	if err := decodeJSON(w, r, &body); err != nil {
		fail(w, r, err)
		return
	}
	if body.Position < 1 {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidBody, "position must be a number from 1", nil)
		return
	}
	noteAudit(r, "queue_id", id)
	noteAudit(r, "position", body.Position)
	items, err := s.acts.MoveQueueItem(r.Context(), id, body.Position)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]QueueItem, 0, len(items))
	for _, it := range items {
		out = append(out, queueItemOf(boardQueueItem(it)))
	}
	writeJSON(w, http.StatusOK, list[QueueItem]{Items: out})
}

func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r.URL.Query().Get("limit"), 50, 1, 200)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "limit must be a number from 1 to 200", nil)
		return
	}
	ops := s.acts.Operations().List(limit)
	out := make([]Operation, 0, len(ops))
	for _, op := range ops {
		out = append(out, operationOf(op))
	}
	writeJSON(w, http.StatusOK, list[Operation]{Items: out})
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	op, ok := s.acts.Operations().Get(r.PathValue("id"))
	if !ok {
		writeProblem(w, r, http.StatusNotFound, codeNotFound, "operation "+r.PathValue("id")+" not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, operationOf(op))
}

// boardQueueItem is a store item as the board types it, so one converter serves both.
func boardQueueItem(it store.QueueItem) board.QueueItem {
	return board.QueueItem{ID: it.ID, Position: it.Position, Repo: it.Repo, Issue: it.Issue, Title: it.Title, AddedAt: it.AddedAt, Waiting: it.Waiting}
}

// queueID reads {id} from the path, answering the problem of a bad one.
func queueID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "the queue item id must be a positive number", nil)
		return 0, false
	}
	return id, true
}
