package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/control"
)

// Pace of a log stream: the files every logPoll, the run's state every logStateCheck, since the
// state is a database read and the files are not.
const (
	logPoll       = 250 * time.Millisecond
	logStateCheck = time.Second
	maxLogTail    = 5000
)

// logsResponse is a page of a run's logs: the lines and where to ask for the next ones.
type logsResponse struct {
	Lines    []LogLine `json:"lines"`
	Cursor   string    `json:"cursor"`
	Finished bool      `json:"finished"`
}

// logChunk is one `log` event of a stream.
type logChunk struct {
	Lines  []LogLine `json:"lines"`
	Cursor string    `json:"cursor"`
}

func (s *Server) logRoutes(mux *router) {
	mux.HandleFunc("GET /api/v1/runs/{id}/logs", s.handleLogs)
	mux.HandleFunc("GET /api/v1/runs/{id}/logs/stream", s.handleLogStream)
}

// logReader opens the logs of run {id} where the request's cursor and roles say.
func (s *Server) logReader(w http.ResponseWriter, r *http.Request) (board.Run, *control.LogReader, bool) {
	id, ok := runID(w, r)
	if !ok {
		return board.Run{}, nil, false
	}
	run, err := s.src.Run(r.Context(), id)
	if errors.Is(err, board.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, codeNotFound, fmt.Sprintf("run %d not found", id), nil)
		return board.Run{}, nil, false
	}
	if err != nil {
		fail(w, r, err)
		return board.Run{}, nil, false
	}
	var roles []string
	if v := r.URL.Query().Get("roles"); v != "" {
		for role := range strings.SplitSeq(v, ",") {
			role = strings.TrimSpace(role)
			if !control.ValidSource(role) {
				writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, "roles must be log source names separated by commas", nil)
				return board.Run{}, nil, false
			}
			roles = append(roles, role)
		}
	}
	cursor, err := control.ParseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		fail(w, r, err)
		return board.Run{}, nil, false
	}
	reader := control.NewLogReader(filepath.Join(run.Dir, "logs"), roles)
	reader.Seek(cursor)
	return run, reader, true
}

// finished reports whether the run's state is terminal, as the board sees it.
func finished(state string) bool {
	return state == "recorded" || state == "needs_info" || state == "halted"
}

// handleLogs answers the lines after ?cursor (or the last ?tail of them), and the cursor to go on.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	run, reader, ok := s.logReader(w, r)
	if !ok {
		return
	}
	tail, err := intParam(r.URL.Query().Get("tail"), 0, 0, maxLogTail)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, codeInvalidParam, fmt.Sprintf("tail must be a number from 0 to %d", maxLogTail), nil)
		return
	}
	done := finished(run.State)
	lines, err := reader.Read(done)
	if err != nil {
		fail(w, r, err)
		return
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	writeJSON(w, http.StatusOK, logsResponse{Lines: logLinesOf(lines), Cursor: reader.Cursor().Encode(), Finished: done})
}

// handleLogStream pushes `log` events as lines are appended, then `end` once the run finished
// and its last lines are out.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	run, reader, ok := s.logReader(w, r)
	if !ok {
		return
	}
	release, ok := s.streams.take()
	if !ok {
		writeProblem(w, r, http.StatusTooManyRequests, codeTooManyStreams, fmt.Sprintf("at most %d streams may be open", s.opts.MaxStreams), nil)
		return
	}
	defer release()
	sse := startSSE(w, r)
	if sse == nil {
		return
	}
	poll := time.NewTicker(s.logPoll)
	defer poll.Stop()
	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	done := finished(run.State)
	lastCheck := time.Now() // wall time: the pace of the checks, not a date anyone sees
	for {
		lines, err := reader.Read(done)
		if err != nil {
			return
		}
		if len(lines) > 0 {
			if err := sse.event("log", logChunk{Lines: logLinesOf(lines), Cursor: reader.Cursor().Encode()}); err != nil {
				return
			}
		}
		if done {
			_ = sse.event("end", map[string]string{"cursor": reader.Cursor().Encode()})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if err := sse.ping(); err != nil {
				return
			}
		case <-poll.C:
		}
		if now := time.Now(); now.Sub(lastCheck) >= s.logStateCheck {
			lastCheck = now
			current, err := s.src.Run(r.Context(), run.ID)
			if err != nil {
				return
			}
			done = finished(current.State)
		}
	}
}

func logLinesOf(in []control.LogLine) []LogLine {
	out := make([]LogLine, 0, len(in))
	for _, l := range in {
		out = append(out, LogLine{TS: l.TS, Source: l.Source, Text: l.Text})
	}
	return out
}
