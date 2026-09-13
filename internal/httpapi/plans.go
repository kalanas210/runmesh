package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// The two planning endpoints, and why there are two.
//
//	POST /api/v1/plans    goal ──▶ a validated plan. Nothing is executed.
//	POST /api/v1/goals    goal ──▶ a validated plan ──▶ a submitted job.
//
// The dry run is not a debugging convenience. A plan is written by a language
// model and then executed against a real cluster; being able to see exactly
// what will run, with the model's own reasoning and every rejected attempt
// beside it, before anything runs, is what makes "never trust raw LLM output"
// something a caller can act on rather than a sentence in a README. It is also
// the endpoint a human-in-the-loop UI is built from: propose, review, submit.
//
// /goals is the same pipeline with the submission on the end, for the
// autonomous case. Both require jobs.write, because both are how work gets
// created — a dry run that anyone with read access could trigger would still
// spend tokens.

// goalRequest is the body both endpoints take.
type goalRequest struct {
	// Goal is UNTRUSTED text. It reaches a model, and it may contain anything,
	// including instructions aimed at that model. See internal/planner/prompt.go
	// for what is done about that and — more usefully — for the list of
	// validators that hold whether or not it works.
	Goal string `json:"goal"`
	// Context is optional structured data the plan may refer to.
	Context json.RawMessage `json:"context,omitempty"`
	// MaxSteps bounds the generated plan, up to the planner's own ceiling.
	MaxSteps int `json:"max_steps,omitempty"`
	// Name overrides the plan's generated name.
	Name string `json:"name,omitempty"`
}

type planResponse struct {
	Plan  *runmesh.Plan `json:"plan"`
	Trace planner.Trace `json:"trace"`
}

type goalResponse struct {
	Job   jobResponse   `json:"job"`
	Plan  *runmesh.Plan `json:"plan"`
	Trace planner.Trace `json:"trace"`
}

// maxGoalBytes bounds the goal text. The body limit already bounds the request;
// this bounds the part that becomes a prompt, because tokens are billed and a
// caller sending a novel should get a 400 rather than an invoice.
const maxGoalBytes = 8 << 10

// createPlan is POST /api/v1/plans: generate and validate, execute nothing.
func (a *API) createPlan(w http.ResponseWriter, r *http.Request) {
	goal, ok := a.decodeGoal(w, r)
	if !ok {
		return
	}

	result, err := a.planner.Plan(r.Context(), goal)
	if err != nil {
		a.writePlannerError(w, r, err, result)
		return
	}
	if goal.Name != "" {
		result.Plan.Name = goal.Name
	}

	a.log.Info("plan generated",
		"request_id", RequestIDFrom(r.Context()),
		"model", result.Trace.Model, "steps", len(result.Plan.Steps),
		"attempts", len(result.Trace.Attempts), "tokens", result.Trace.TotalTokens)

	writeJSON(w, a.log, http.StatusOK, planResponse{Plan: result.Plan, Trace: result.Trace})
}

// createGoal is POST /api/v1/goals: generate, validate, and submit.
//
// The generated plan is submitted through submitPlan — the same function
// createJob uses — rather than through a path of its own. That is the whole
// argument of Week 5 in one line: an LLM-authored plan is admitted by exactly
// the code that admits a curl request, including the queue-depth check, the
// per-tool validation, the execution policy and the idempotency replay. A
// second submission path would be a second place for the gates to be missing.
func (a *API) createGoal(w http.ResponseWriter, r *http.Request) {
	goal, ok := a.decodeGoal(w, r)
	if !ok {
		return
	}

	result, err := a.planner.Plan(r.Context(), goal)
	if err != nil {
		a.writePlannerError(w, r, err, result)
		return
	}
	if goal.Name != "" {
		result.Plan.Name = goal.Name
	}

	stored, replayed, err := a.submitPlan(r, result.Plan)
	if err != nil {
		writeError(w, r, a.log, err)
		return
	}

	a.log.Info("goal accepted",
		"request_id", RequestIDFrom(r.Context()),
		"job_id", stored.ID, "model", result.Trace.Model,
		"steps", len(stored.Steps), "tokens", result.Trace.TotalTokens)

	status := http.StatusCreated
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		status = http.StatusOK
	}
	w.Header().Set("Location", "/api/v1/jobs/"+stored.ID)
	writeJSON(w, a.log, status, goalResponse{
		Job:   toJobResponse(stored),
		Plan:  result.Plan,
		Trace: result.Trace,
	})
}

// decodeGoal reads and checks the request body.
func (a *API) decodeGoal(w http.ResponseWriter, r *http.Request) (planner.Goal, bool) {
	if a.planner == nil {
		writeError(w, r, a.log, errNoPlanner)
		return planner.Goal{}, false
	}

	var body goalRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, r, a.log, err)
		return planner.Goal{}, false
	}
	if len(body.Goal) > maxGoalBytes {
		writeError(w, r, a.log, badRequest(fmt.Sprintf(
			"goal is %d bytes; the limit is %d", len(body.Goal), maxGoalBytes)))
		return planner.Goal{}, false
	}
	if len(body.Context) > 0 && !json.Valid(body.Context) {
		writeError(w, r, a.log, badRequest("context is not valid JSON"))
		return planner.Goal{}, false
	}
	if body.MaxSteps < 0 {
		writeError(w, r, a.log, badRequest("max_steps must not be negative"))
		return planner.Goal{}, false
	}
	return planner.Goal{
		Text:     body.Goal,
		Context:  body.Context,
		MaxSteps: body.MaxSteps,
		Name:     body.Name,
	}, true
}

// writePlannerError maps a planning failure onto a status and the envelope,
// with the trace beside it.
func (a *API) writePlannerError(w http.ResponseWriter, r *http.Request, err error, result planner.Result) {
	var ve *runmesh.ValidationError
	if errors.As(err, &ve) {
		a.writePlannerFailure(w, r, err, result, http.StatusUnprocessableEntity, APIError{
			Code:    CodeUnprocessable,
			Message: "the planner could not produce a valid plan for this goal",
			Details: ve.Details,
		})
		return
	}
	if errors.Is(err, planner.ErrNoTools) {
		writeError(w, r, a.log, badRequest(
			"no tools are available to plan with; every registered tool is refused "+
				"by the execution policy"))
		return
	}

	// Everything else is the model or the transport, classified by the model's
	// adapter, and each class is answered as what it is rather than flattened
	// into "internal error". A caller that hit a rate limit is told when to come
	// back; one whose goal was refused is told the request was fine and the
	// goal was not; and a deployment whose model no longer exists says so,
	// instead of looking like a RunMesh bug with the reason only in the log.
	//
	// The provider's code is stable, low-cardinality and documented as safe to
	// show a caller, so it goes in details. The provider's message does not: it
	// is written by somebody else, and it stays in the log.
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		writeError(w, r, a.log, err)
		return
	}
	details := []runmesh.Detail{{Field: "planner", Issue: te.Code}}
	switch {
	case errors.Is(err, planner.ErrModelRefused):
		a.writePlannerFailure(w, r, err, result, http.StatusUnprocessableEntity, APIError{
			Code:    CodeUnprocessable,
			Message: "the planning model refused this goal; rephrasing it may help, repeating it will not",
			Details: details,
		})
	case errors.Is(err, planner.ErrModelRateLimited):
		w.Header().Set("Retry-After", retryAfterSeconds(te.RetryAfter))
		a.writePlannerFailure(w, r, err, result, http.StatusTooManyRequests, APIError{
			Code:    CodeResourceExhausted,
			Message: "the planning model's rate limit was reached; retry after the Retry-After interval",
			Details: details,
		})
	case errors.Is(err, planner.ErrModelUnusable):
		// No Retry-After, and the absence is part of the answer: waiting does
		// not bring back a model the provider has stopped serving.
		a.writePlannerFailure(w, r, err, result, http.StatusServiceUnavailable, APIError{
			Code: CodeUnavailable,
			Message: "planning is unavailable: this deployment's model settings do not work " +
				"(a rejected key, or a model the provider no longer serves), and no retry " +
				"helps until an operator changes them",
			Details: details,
		})
	default:
		if te.Retryable {
			w.Header().Set("Retry-After", retryAfterSeconds(te.RetryAfter))
		}
		a.writePlannerFailure(w, r, err, result, http.StatusServiceUnavailable, APIError{
			Code:    CodeUnavailable,
			Message: "the planning model did not produce an answer",
			Details: details,
		})
	}
}

// writePlannerFailure is writeError for the planning endpoints: the same
// envelope and the same logging rule, with the trace attached.
//
// The trace is attached even on failure, and especially on failure: it carries
// every attempt the model made and every problem the validator found with each
// one. Without it a refused goal is a 4xx about a plan the caller never saw.
func (a *API) writePlannerFailure(w http.ResponseWriter, r *http.Request, err error,
	result planner.Result, status int, api APIError) {

	api.RequestID = RequestIDFrom(r.Context())
	switch {
	case status >= http.StatusInternalServerError:
		a.log.Error("request failed", "status", status, "code", api.Code, "err", err)
	case status == http.StatusTooManyRequests:
		// A quota running out is worth seeing before the complaints arrive.
		a.log.Warn("planning model rate limited", "status", status, "err", err)
	default:
		a.log.Debug("request rejected", "status", status, "code", api.Code, "err", err)
	}
	writeJSON(w, a.log, status, plannerErrorEnvelope{Error: api, Trace: result.Trace})
}

// retryAfterSeconds renders a Retry-After value in whole seconds, rounded up
// and never zero: "Retry-After: 0" invites exactly the tight loop the header
// exists to prevent.
func retryAfterSeconds(d time.Duration) string {
	return strconv.Itoa(max(int((d+time.Second-1)/time.Second), 1))
}

type plannerErrorEnvelope struct {
	Error APIError      `json:"error"`
	Trace planner.Trace `json:"trace"`
}

var errNoPlanner = errors.New("httpapi: no planner is configured")
