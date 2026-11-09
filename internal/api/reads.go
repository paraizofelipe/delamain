package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/paraizo/delamain/internal/board"
)

// Bounds of a runs page.
const (
	defaultRunsLimit = 50
	maxRunsLimit     = 200
)

// readRoutes are the routes that only read.
func (s *Server) readRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/repos", s.handleRepos)
	mux.HandleFunc("GET /api/v1/runs", s.handleRuns)
	mux.HandleFunc("GET /api/v1/runs/{id}", s.handleRun)
	mux.HandleFunc("GET /api/v1/queue", s.handleQueue)
	mux.HandleFunc("GET /api/v1/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/board", s.handleBoard)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.src.Status(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.statusOf(st, true))
}

// statusOf converts the board status; dated says whether to stamp the server time, which the
// event stream leaves out so an unchanged status stays unchanged.
func (s *Server) statusOf(st board.Status, dated bool) Status {
	out := Status{
		Daemon: st.Daemon, SlotsInUse: st.SlotsInUse, SlotsMax: st.SlotsMax, QueueRunning: st.QueueRunning,
		Today: st.TodaySpend, TodayLevel: st.TodayLevel, Version: s.opts.Version,
	}
	if dated {
		out.ServerTime = s.opts.Now()
	}
	return out
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.src.Repos(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	if repos == nil {
		repos = []Repo{}
	}
	writeJSON(w, http.StatusOK, list[Repo]{Items: repos})
}

// handleRuns pages the runs newest first: ?state=active|finished|all, ?limit, ?before=<id>.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := q.Get("state")
	switch filter {
	case "":
		filter = "all"
	case "active", "finished", "all":
	default:
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "state must be one of active|finished|all", nil)
		return
	}
	limit, err := intParam(q.Get("limit"), defaultRunsLimit, 1, maxRunsLimit)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "limit must be a number from 1 to 200", nil)
		return
	}
	before, err := intParam(q.Get("before"), 0, 1, math.MaxInt)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "before must be a run id", nil)
		return
	}
	runs, err := s.src.RunsBefore(r.Context(), int64(before), limit+1, filter)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := list[RunSummary]{Items: make([]RunSummary, 0, len(runs))}
	for i, run := range runs {
		if i == limit {
			next := runs[i-1].ID
			out.NextBefore = &next
			break
		}
		out.Items = append(out.Items, summaryOf(run, s.board))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	run, err := s.src.Run(r.Context(), id)
	if errors.Is(err, board.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, codeNotFound, "run "+strconv.FormatInt(id, 10)+" not found", nil)
		return
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	d, err := s.board.Detail(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, detailOf(run, d, s.board))
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	items, err := s.src.Queue(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list[QueueItem]{Items: queueOf(items)})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	q, err := board.ParseReportQuery(r.URL.Query())
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, err.Error(), nil)
		return
	}
	rep, err := s.src.Report(r.Context(), q)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleBoard is the kanban projection the local board shows, for an app that wants it as is.
func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	snap, err := s.board.Snapshot(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// runID reads {id} from the path, answering the problem of a bad one.
func runID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "the run id must be a positive number", nil)
		return 0, false
	}
	return id, true
}

// intParam parses v as an integer in [lo, hi], or def when empty.
func intParam(v string, def, lo, hi int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, errors.New("out of range")
	}
	return n, nil
}
