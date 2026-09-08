package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Stable, machine-readable error codes. They are borrowed from the gRPC/Google
// API vocabulary because it is small, widely understood, and maps cleanly onto
// HTTP status codes without inventing a private taxonomy.
const (
	CodeInvalidArgument    = "invalid_argument"
	CodeUnauthenticated    = "unauthenticated"
	CodePermissionDenied   = "permission_denied"
	CodeNotFound           = "not_found"
	CodeFailedPrecondition = "failed_precondition"
	CodeResourceExhausted  = "resource_exhausted"
	CodeInternal           = "internal"
	// CodeUnprocessable: the request was well-formed and the runtime could not
	// produce a result from it. It carries a 422 rather than a 400 because the
	// caller's input was fine — a goal the planner could not turn into a valid
	// plan is not a malformed request, and telling a client to "fix the syntax"
	// of a perfectly good sentence sends it in the wrong direction.
	CodeUnprocessable = "unprocessable"
	// CodeUnimplemented: this deployment does not have the feature. 501, not
	// 500: nothing is broken, and no retry will help until an operator
	// configures it.
	CodeUnimplemented = "unimplemented"
)

// APIError is the one and only non-2xx body shape. Never a bare string, never
// a stack trace, always a request id the caller can quote in a bug report.
type APIError struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	Details   []runmesh.Detail `json:"details,omitempty"`
	RequestID string           `json:"request_id,omitempty"`
}

type errorEnvelope struct {
	Error APIError `json:"error"`
}

// writeJSON writes a value as JSON with the given status.
func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own DTO failed, so the response body is already
		// beyond saving. Headers may not have been written yet, so a 500 with
		// a hand-built body is still possible and is better than a truncated
		// one.
		log.Error("could not marshal response", "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError maps a domain error onto the envelope. It is the single place
// status codes are decided, so a new endpoint cannot invent its own mapping.
//
// The cause is logged and never serialised: a client learns what it did wrong
// and nothing about the internals it did it to.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, api := classifyError(err)
	api.RequestID = RequestIDFrom(r.Context())

	if status >= http.StatusInternalServerError {
		log.Error("request failed", "status", status, "err", err)
	} else {
		log.Debug("request rejected", "status", status, "code", api.Code, "err", err)
	}
	writeJSON(w, log, status, errorEnvelope{Error: api})
}

func classifyError(err error) (int, APIError) {
	var ve *runmesh.ValidationError
	switch {
	case errors.As(err, &ve):
		return http.StatusBadRequest, APIError{
			Code:    CodeInvalidArgument,
			Message: "the submitted plan is not valid",
			Details: ve.Details,
		}

	case errors.Is(err, errUnauthenticated):
		// Deliberately says nothing about which part failed: a missing header,
		// a wrong scheme and a wrong key are indistinguishable to a caller
		// probing for valid keys.
		return http.StatusUnauthorized, APIError{
			Code:    CodeUnauthenticated,
			Message: "a valid API key is required",
		}

	case errors.As(err, new(*permissionError)):
		// 403, not 401, and the difference matters to a client: the credential
		// was accepted, so retrying with the same key will never work. Naming
		// the missing scope tells an already-authenticated caller what to ask
		// their operator for.
		var pe *permissionError
		errors.As(err, &pe)
		return http.StatusForbidden, APIError{
			Code:    CodePermissionDenied,
			Message: "this API key does not carry the " + string(pe.scope) + " scope",
		}

	case errors.Is(err, runmesh.ErrNotFound):
		return http.StatusNotFound, APIError{Code: CodeNotFound, Message: "not found"}

	case errors.Is(err, runmesh.ErrConflict):
		return http.StatusConflict, APIError{
			Code:    CodeFailedPrecondition,
			Message: "the resource is not in a state that allows this operation",
		}

	case errors.Is(err, runmesh.ErrQueueFull):
		return http.StatusTooManyRequests, APIError{
			Code:    CodeResourceExhausted,
			Message: "the queue is full; retry shortly",
		}

	case errors.Is(err, errRequestTooLarge):
		return http.StatusRequestEntityTooLarge, APIError{
			Code:    CodeInvalidArgument,
			Message: "the request body is too large",
		}

	case errors.Is(err, errNoPlanner):
		return http.StatusNotImplemented, APIError{
			Code: CodeUnimplemented,
			Message: "this deployment has no planner configured; " +
				"submit a plan to POST /api/v1/jobs, or set RUNMESH_PLANNER",
		}

	case errors.As(err, new(*badRequestError)):
		var bre *badRequestError
		errors.As(err, &bre)
		return http.StatusBadRequest, APIError{Code: CodeInvalidArgument, Message: bre.msg}

	default:
		// runmesh.ErrClosed and anything unmapped. A generic message: the
		// cause is in the log, correlated by request id.
		return http.StatusInternalServerError, APIError{
			Code:    CodeInternal,
			Message: "internal error",
		}
	}
}

// badRequestError is a client mistake that is not a plan validation failure —
// a malformed cursor, an unparseable body, an unknown query parameter value.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func badRequest(msg string) error { return &badRequestError{msg: msg} }

// permissionError is an authenticated caller asking for something its key does
// not cover.
type permissionError struct{ scope config.Scope }

func (e *permissionError) Error() string {
	return "httpapi: the API key lacks the " + string(e.scope) + " scope"
}

var (
	errUnauthenticated = errors.New("httpapi: unauthenticated")
	errRequestTooLarge = errors.New("httpapi: request body too large")
)
