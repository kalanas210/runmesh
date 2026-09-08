package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

const maxIdempotencyKeyLen = 128

// createJob is POST /api/v1/jobs.
//
// The body decodes directly into runmesh.Plan — the same struct the Week-5
// Gemini planner will produce — so an LLM-authored plan passes through this
// exact validation gate with no new code. That is what "never trust raw model
// output" costs when the contract is designed for it up front: nothing.
func (a *API) createJob(w http.ResponseWriter, r *http.Request) {
	var plan runmesh.Plan
	if err := decodeJSON(r, &plan); err != nil {
		writeError(w, r, a.log, err)
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if len(idemKey) > maxIdempotencyKeyLen {
		writeError(w, r, a.log, badRequest("Idempotency-Key is too long"))
		return
	}

	if err := plan.Validate(a.tools.Has, a.limits); err != nil {
		writeError(w, r, a.log, err)
		return
	}
	// Per-tool validation and the execution policy both run at submit time, so
	// a malformed parameter or a tool this deployment refuses is a 400 now
	// rather than a step that fails three times in an hour.
	//
	// Both are reported through the same per-field envelope, and every problem
	// is collected rather than the first one returned. That matters far more
	// when the author is a language model retrying in a loop than when it is a
	// human: one response says everything that is wrong with the plan, so the
	// repair pass in Week 5 has something complete to work from.
	var details []runmesh.Detail
	for i, step := range plan.Steps {
		if err := a.tools.Validate(step.Tool, step.Params); err != nil {
			details = append(details, runmesh.Detail{
				Field: fmt.Sprintf("steps[%d].params", i),
				Issue: err.Error(),
			})
		}
		if a.policy != nil {
			if err := a.policy.Allows(step.Tool); err != nil {
				details = append(details, runmesh.Detail{
					Field: fmt.Sprintf("steps[%d].tool", i),
					Issue: policyIssue(err),
				})
			}
		}
	}
	if len(details) > 0 {
		writeError(w, r, a.log, &runmesh.ValidationError{Details: details})
		return
	}

	stored, replayed, err := a.submitPlan(r, &plan)
	if err != nil {
		if errors.Is(err, runmesh.ErrQueueFull) {
			w.Header().Set("Retry-After", "1")
		}
		writeError(w, r, a.log, err)
		return
	}
	if replayed {
		// A replay is not an error. Returning the original job with 200 is what
		// makes a client's retry after a network timeout safe.
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("Location", "/api/v1/jobs/"+stored.ID)
		writeJSON(w, a.log, http.StatusOK, toJobResponse(stored))
		return
	}

	a.log.Info("job accepted",
		"request_id", RequestIDFrom(r.Context()),
		"job_id", stored.ID, "name", stored.Name, "steps", len(stored.Steps))

	w.Header().Set("Location", "/api/v1/jobs/"+stored.ID)
	writeJSON(w, a.log, http.StatusCreated, toJobResponse(stored))
}

// submitPlan is admission and persistence, shared by POST /api/v1/jobs and
// POST /api/v1/goals.
//
// It is factored out precisely so a generated plan cannot take a different
// route in. Admission control, id minting, the defaults, the idempotency
// replay: a model-authored plan gets all of them because it is the same
// function, not because somebody remembered to add them to a second handler.
func (a *API) submitPlan(r *http.Request, plan *runmesh.Plan) (job *runmesh.Job, replayed bool, err error) {
	now := a.clock.Now()

	// Admission control before persistence: a runtime that accepts unbounded
	// work is a runtime that falls over politely instead of pushing back.
	depth, err := a.store.QueueDepth(r.Context(), now)
	if err != nil {
		return nil, false, err
	}
	if a.maxQueueDepth > 0 && depth+len(plan.Steps) > a.maxQueueDepth {
		return nil, false, fmt.Errorf("%w: depth %d", runmesh.ErrQueueFull, depth)
	}

	built := plan.Build(runmesh.NewID("job_", now), now, a.defaults)

	stored, err := a.store.CreateJob(r.Context(), built, r.Header.Get("Idempotency-Key"))
	switch {
	case errors.Is(err, runmesh.ErrDuplicate):
		return stored, true, nil
	case err != nil:
		return nil, false, err
	}
	return stored, false, nil
}

// listJobs is GET /api/v1/jobs, with keyset pagination.
func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := runmesh.JobFilter{Cursor: q.Get("cursor"), Limit: 50}
	for _, raw := range q["state"] {
		st, err := runmesh.ParseState(raw)
		if err != nil {
			writeError(w, r, a.log, badRequest("unknown state "+strconv.Quote(raw)))
			return
		}
		filter.States = append(filter.States, st)
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, r, a.log, badRequest("limit must be an integer in [1, 200]"))
			return
		}
		filter.Limit = n
	}

	page, err := a.store.ListJobs(r.Context(), filter)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}

	out := jobListResponse{Jobs: make([]jobResponse, 0, len(page.Jobs)), NextCursor: page.NextCursor}
	for _, j := range page.Jobs {
		out.Jobs = append(out.Jobs, toJobResponse(j))
	}
	writeJSON(w, a.log, http.StatusOK, out)
}

// getJob is GET /api/v1/jobs/{id}.
func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.Job(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}
	// The version doubles as an ETag, which is what lets the Week-6 dashboard
	// poll cheaply and lets a future conditional update be a one-line change.
	w.Header().Set("ETag", strconv.FormatUint(job.Version, 10))
	writeJSON(w, a.log, http.StatusOK, toJobResponse(job))
}

// cancelJob is POST /api/v1/jobs/{id}/cancel.
//
// It answers 202, not 200: cancellation is a REQUEST. Steps a worker already
// owns keep running until their next heartbeat delivers the news, so the body
// may honestly say "state": "RUNNING" with cancel_requested_at set. That is
// the only answer that stays true now that the cancel can land on a
// different replica from the one executing the step.
func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := a.store.RequestCancel(r.Context(), r.PathValue("id"),
		runmesh.CancelUser, a.clock.Now())
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}
	a.log.Info("cancel requested",
		"request_id", RequestIDFrom(r.Context()), "job_id", job.ID, "state", job.State.String())
	writeJSON(w, a.log, http.StatusAccepted, toJobResponse(job))
}

// jobEvents is GET /api/v1/jobs/{id}/events: the execution timeline that
// becomes the waterfall in the dashboard.
func (a *API) jobEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var after uint64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, r, a.log, badRequest("after must be a non-negative integer"))
			return
		}
		after = n
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, r, a.log, badRequest("limit must be an integer in [1, 1000]"))
			return
		}
		limit = n
	}

	page, err := a.store.JobEvents(r.Context(), r.PathValue("id"), after, limit)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}
	if page.Events == nil {
		page.Events = []runmesh.Event{}
	}
	writeJSON(w, a.log, http.StatusOK, eventsResponse{
		Events:    page.Events,
		NextAfter: page.NextAfter,
		Truncated: page.Truncated,
		OldestSeq: page.OldestSeq,
	})
}

// policyIssue renders a policy refusal for the 400 body.
//
// The classified code is kept and put FIRST, because it is the stable,
// low-cardinality half — a client can branch on tool_denied without parsing
// English, and the sentence after it is what a human needs to know which
// variable to change.
func policyIssue(err error) string {
	var te *runmesh.ToolError
	if errors.As(err, &te) {
		return te.Code + ": " + te.Message
	}
	return err.Error()
}

// decodeJSON reads exactly one JSON value into v.
//
// DisallowUnknownFields turns a typo into a 400 rather than a silently ignored
// field — which matters far more when the author is an LLM that will keep
// making the same mistake than when it is a human who would notice.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return badRequest("a request body is required")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errRequestTooLarge
		}
		var syn *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.Is(err, io.EOF):
			return badRequest("a request body is required")
		case errors.As(err, &syn):
			return badRequest(fmt.Sprintf("malformed JSON at byte %d", syn.Offset))
		case errors.As(err, &typ):
			return badRequest(fmt.Sprintf("field %q expects %s", typ.Field, typ.Type))
		default:
			return badRequest(err.Error())
		}
	}
	// A second value in the body means the client sent something other than
	// what it thinks it sent.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return badRequest("the body must contain exactly one JSON object")
	}
	return nil
}
