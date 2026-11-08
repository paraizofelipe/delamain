package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/paraizo/delamain/internal/control"
	"github.com/paraizo/delamain/internal/forge"
)

// Problem codes: stable, machine-readable, what the app switches on and localizes.
const (
	codeUnauthorized     = "unauthorized"
	codeRateLimited      = "rate_limited"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeInvalidParam     = "invalid_param"
	codeInvalidBody      = "invalid_body"
	codeInvalidCursor    = "invalid_cursor"
	codePayloadTooLarge  = "payload_too_large"
	codeTooManyStreams   = "too_many_streams"
	codeRepoNotAllowed   = "repo_not_allowed"
	codeRunActive        = "run_active"
	codeRunFinished      = "run_finished"
	codeRunNotFinished   = "run_not_finished"
	codeOwnerAlive       = "owner_alive"
	codeWorktreeDirty    = "worktree_dirty"
	codeWorktreeInUse    = "worktree_in_use"
	codeReviewDisabled   = "review_disabled"
	codeIssueClosed      = "issue_closed"
	codeIssueNotFound    = "issue_not_found"
	codeNoForge          = "no_forge"
	codeForgeError       = "forge_error"
	codeInternal         = "internal"
)

// Codes lists every problem code the API emits, for the OpenAPI document to enumerate.
var Codes = []string{
	codeUnauthorized, codeRateLimited, codeNotFound, codeMethodNotAllowed, codeInvalidParam, codeInvalidBody,
	codeInvalidCursor, codePayloadTooLarge, codeTooManyStreams, codeRepoNotAllowed, codeRunActive, codeRunFinished,
	codeRunNotFinished, codeOwnerAlive, codeWorktreeDirty, codeWorktreeInUse, codeReviewDisabled, codeIssueClosed,
	codeIssueNotFound, codeNoForge, codeForgeError, codeInternal,
}

// Problem is an error answer, as RFC 9457 shapes it, with a code the app can switch on.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code"`
	// Extra carries what the code needs beside the text: run_id, state, worktree...
	Extra map[string]any `json:"-"`
}

// MarshalJSON flattens Extra beside the fixed members.
func (p Problem) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": p.Type, "title": p.Title, "status": p.Status, "code": p.Code}
	if p.Detail != "" {
		out["detail"] = p.Detail
	}
	if p.Instance != "" {
		out["instance"] = p.Instance
	}
	for k, v := range p.Extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// writeProblem answers a problem+json with status and code.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, extra map[string]any) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail,
		Instance: r.URL.Path, Code: code, Extra: extra,
	})
	noteAudit(r, "error", code)
}

// problemError is an error a handler wants answered as a given problem.
type problemError struct {
	status int
	code   string
	detail string
	extra  map[string]any
}

func (e *problemError) Error() string { return e.detail }

// fail answers err as the problem it maps to.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	p := mapErr(err)
	writeProblem(w, r, p.status, p.code, p.detail, p.extra)
}

// mapErr turns the typed errors of control and the forge into problems.
func mapErr(err error) *problemError {
	var pe *problemError
	if errors.As(err, &pe) {
		return pe
	}
	var re *control.RunError
	var ie *control.IssueError
	var apiErr *forge.APIError
	switch {
	case errors.Is(err, control.ErrNotFound):
		return &problemError{http.StatusNotFound, codeNotFound, err.Error(), nil}
	case errors.Is(err, control.ErrInvalid):
		return &problemError{http.StatusBadRequest, codeInvalidParam, err.Error(), nil}
	case errors.Is(err, control.ErrInvalidCursor):
		return &problemError{http.StatusBadRequest, codeInvalidCursor, err.Error(), nil}
	case errors.Is(err, control.ErrRepoNotAllowed):
		return &problemError{http.StatusForbidden, codeRepoNotAllowed, err.Error(), nil}
	case errors.Is(err, control.ErrReviewDisabled):
		return &problemError{http.StatusConflict, codeReviewDisabled, err.Error(), nil}
	case errors.Is(err, control.ErrNoForge):
		return &problemError{http.StatusConflict, codeNoForge, err.Error(), nil}
	case errors.Is(err, control.ErrIssueClosed):
		extra := map[string]any{}
		if errors.As(err, &ie) {
			extra["issue"] = ie.Issue
		}
		return &problemError{http.StatusConflict, codeIssueClosed, err.Error(), extra}
	case errors.Is(err, control.ErrRunActive) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeRunActive, err.Error(), map[string]any{"run_id": re.ID, "state": string(re.State)}}
	case errors.Is(err, control.ErrRunFinished) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeRunFinished, err.Error(), map[string]any{"run_id": re.ID, "state": string(re.State)}}
	case errors.Is(err, control.ErrRunNotFinished) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeRunNotFinished, err.Error(), map[string]any{"run_id": re.ID, "state": string(re.State)}}
	case errors.Is(err, control.ErrOwnerAlive) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeOwnerAlive, err.Error(), map[string]any{"run_id": re.ID, "owner_pid": re.OwnerPID}}
	case errors.Is(err, control.ErrDirtyWorktree) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeWorktreeDirty, err.Error(), map[string]any{"run_id": re.ID, "worktree": re.Worktree}}
	case errors.Is(err, control.ErrWorktreeInUse) && errors.As(err, &re):
		return &problemError{http.StatusConflict, codeWorktreeInUse, err.Error(), map[string]any{"run_id": re.UsedBy, "worktree": re.Worktree, "state": string(re.State)}}
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return &problemError{http.StatusNotFound, codeIssueNotFound, err.Error(), nil}
	case errors.As(err, &apiErr):
		return &problemError{http.StatusBadGateway, codeForgeError, err.Error(), map[string]any{"forge_status": apiErr.Status}}
	}
	return &problemError{http.StatusInternalServerError, codeInternal, err.Error(), nil}
}
