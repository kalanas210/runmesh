package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/config"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/planner"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// stubPlanner answers with a fixed plan, or a fixed error.
type stubPlanner struct {
	result planner.Result
	err    error
	goals  []planner.Goal
}

func (s *stubPlanner) Plan(_ context.Context, goal planner.Goal) (planner.Result, error) {
	s.goals = append(s.goals, goal)
	return s.result, s.err
}

func withPlanner(p httpapi.Planner) func(*httpapi.Deps) {
	return func(d *httpapi.Deps) { d.Planner = p }
}

func samplePlan() *runmesh.Plan {
	return &runmesh.Plan{
		Name: "generated",
		Steps: []runmesh.PlanStep{
			{ID: "a", Tool: "echo", Params: json.RawMessage(`{"message":"hi"}`)},
			{ID: "b", Tool: "report_generate", DependsOn: []string{"a"}},
		},
	}
}

func sampleResult() planner.Result {
	return planner.Result{
		Plan: samplePlan(),
		Trace: planner.Trace{
			Model:       "scripted",
			Reasoning:   "one step, then a report",
			Tools:       []string{"echo", "report_generate"},
			TotalTokens: 210,
			Attempts:    []planner.Attempt{{N: 1, Accepted: true}},
		},
	}
}

// TestPlansEndpointExecutesNothing.
//
// The dry run is not a debugging convenience: it is what lets a caller — or a
// human at a screen — see exactly what a language model proposes before any of
// it runs. If it created a job it would be /goals with a confusing name.
func TestPlansEndpointExecutesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t, withPlanner(&stubPlanner{result: sampleResult()}))
	rec := f.do(http.MethodPost, "/api/v1/plans", `{"goal":"say hi and report"}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Plan  *runmesh.Plan `json:"plan"`
		Trace planner.Trace `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Plan == nil || len(body.Plan.Steps) != 2 {
		t.Fatalf("plan = %+v, want the generated one", body.Plan)
	}
	if body.Trace.Model != "scripted" || body.Trace.Reasoning == "" {
		t.Errorf("trace = %+v, want the model and its reasoning: a plan nobody "+
			"can see the provenance of is not reviewable", body.Trace)
	}

	page, err := f.store.ListJobs(t.Context(), runmesh.JobFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Errorf("the dry run created %d jobs", len(page.Jobs))
	}
}

// TestGoalsEndpointSubmitsThroughTheSameGate.
//
// The generated plan goes through submitPlan — the same function POST
// /api/v1/jobs uses — so admission control, id minting, the defaults and the
// idempotency replay all apply to a model-authored plan because it is the same
// code, not because somebody remembered to add them twice.
func TestGoalsEndpointSubmitsThroughTheSameGate(t *testing.T) {
	t.Parallel()

	p := &stubPlanner{result: sampleResult()}
	f := newFixture(t, withPlanner(p))

	rec := f.do(http.MethodPost, "/api/v1/goals", `{"goal":"say hi and report"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/api/v1/jobs/job_") {
		t.Errorf("Location = %q, want the created job", loc)
	}

	var body struct {
		Job struct {
			ID    string `json:"id"`
			State string `json:"state"`
			Steps []struct {
				ID   string `json:"id"`
				Tool string `json:"tool"`
			} `json:"steps"`
		} `json:"job"`
		Trace planner.Trace `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Job.ID == "" || len(body.Job.Steps) != 2 {
		t.Fatalf("job = %+v, want the plan persisted", body.Job)
	}
	if body.Trace.TotalTokens != 210 {
		t.Errorf("trace tokens = %d; the cost of a submitted goal must be visible",
			body.Trace.TotalTokens)
	}

	// The goal reached the planner intact.
	if len(p.goals) != 1 || p.goals[0].Text != "say hi and report" {
		t.Errorf("the planner received %+v", p.goals)
	}

	stored, err := f.store.Job(t.Context(), body.Job.ID)
	if err != nil {
		t.Fatalf("the job was not persisted: %v", err)
	}
	if len(stored.Steps) != 2 || stored.Steps[1].DependsOn[0] != "a" {
		t.Errorf("the DAG did not survive submission: %+v", stored.Steps)
	}
}

// TestAnUnplannableGoalIs422WithTheTrace.
//
// 422 rather than 400: the caller's request was fine. And the trace is attached
// especially on failure, because without it a refused goal is a message about a
// plan the caller never saw.
func TestAnUnplannableGoalIs422WithTheTrace(t *testing.T) {
	t.Parallel()

	f := newFixture(t, withPlanner(&stubPlanner{
		err: &runmesh.ValidationError{Details: []runmesh.Detail{
			{Field: "steps[0].tool", Issue: "unknown_tool"},
		}},
		result: planner.Result{Trace: planner.Trace{
			Model: "scripted",
			Attempts: []planner.Attempt{
				{N: 1, Problems: []runmesh.Detail{{Field: "steps[0].tool", Issue: "unknown_tool"}}},
				{N: 2, Problems: []runmesh.Detail{{Field: "steps[0].tool", Issue: "unknown_tool"}}},
			},
		}},
	}))

	rec := f.do(http.MethodPost, "/api/v1/goals", `{"goal":"do something impossible"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. Body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Code    string           `json:"code"`
			Details []runmesh.Detail `json:"details"`
		} `json:"error"`
		Trace planner.Trace `json:"trace"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Error.Details) == 0 {
		t.Error("the 422 names no field")
	}
	if len(body.Trace.Attempts) != 2 {
		t.Errorf("the response carries %d attempts, want both: a refused goal has "+
			"to be diagnosable", len(body.Trace.Attempts))
	}
}

// TestPlanningWithoutAPlannerIs501. Not 404 — the route exists — and not 500:
// nothing is broken and no retry helps until an operator configures one.
func TestPlanningWithoutAPlannerIs501(t *testing.T) {
	t.Parallel()

	f := newFixture(t) // no planner
	for _, path := range []string{"/api/v1/plans", "/api/v1/goals"} {
		rec := f.do(http.MethodPost, path, `{"goal":"x"}`, nil)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: status = %d, want 501", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "RUNMESH_PLANNER") {
			t.Errorf("%s: the response does not name the variable to set: %s",
				path, rec.Body.String())
		}
	}
}

// TestPlanningRequiresJobsWrite, including the dry run. /plans executes
// nothing, but it spends model tokens, and a capability that costs money is a
// write however little it changes.
func TestPlanningRequiresJobsWrite(t *testing.T) {
	t.Parallel()

	f := scopedFixture(t, config.ScopeJobsRead)
	for _, path := range []string{"/api/v1/plans", "/api/v1/goals"} {
		rec := f.do(http.MethodPost, path, `{"goal":"x"}`, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403 for a read-only key", path, rec.Code)
		}
	}
}

// TestGoalValidation rejects what should never reach a model.
func TestGoalValidation(t *testing.T) {
	t.Parallel()

	p := &stubPlanner{result: sampleResult()}
	f := newFixture(t, withPlanner(p))

	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"a goal":             {`{"goal":"do a thing"}`, http.StatusOK},
		"with context":       {`{"goal":"do a thing","context":{"rows":3}}`, http.StatusOK},
		"no body":            {``, http.StatusBadRequest},
		"empty object":       {`{}`, http.StatusOK}, // an empty goal is the planner's to refuse
		"unknown field":      {`{"goal":"x","temperature":0.9}`, http.StatusBadRequest},
		"negative max_steps": {`{"goal":"x","max_steps":-1}`, http.StatusBadRequest},
		// The body limit is the outer bound and it fires first, which is the
		// correct order: nothing should be read into memory to discover it was
		// too big. The goal-specific cap is exercised below.
		"oversized body": {`{"goal":"` + strings.Repeat("a", 9000) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		rec := f.do(http.MethodPost, "/api/v1/plans", tc.body, nil)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d. Body: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// TestAnOversizedGoalIsRejectedBeforeItReachesTheModel. The request body limit
// bounds the request; this bounds the part that becomes a prompt, because
// tokens are billed and a caller sending a novel should get a 400 rather than
// an invoice.
func TestAnOversizedGoalIsRejectedBeforeItReachesTheModel(t *testing.T) {
	t.Parallel()

	p := &stubPlanner{result: sampleResult()}
	f := newFixture(t, withPlanner(p), func(d *httpapi.Deps) {
		d.MaxRequestBytes = 1 << 20 // well above the goal cap, so it is the goal cap that fires
	})

	rec := f.do(http.MethodPost, "/api/v1/plans",
		`{"goal":"`+strings.Repeat("a", 9000)+`"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
	if len(p.goals) != 0 {
		t.Error("the oversized goal reached the planner, and therefore the model")
	}
}

// TestTheGoalsNameOverrideReachesTheJob.
func TestTheGoalsNameOverrideReachesTheJob(t *testing.T) {
	t.Parallel()

	f := newFixture(t, withPlanner(&stubPlanner{result: sampleResult()}))
	rec := f.do(http.MethodPost, "/api/v1/goals",
		`{"goal":"say hi","name":"nightly summary"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Job struct {
			Name string `json:"name"`
		} `json:"job"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Job.Name != "nightly summary" {
		t.Errorf("job name = %q, want the caller's override", body.Job.Name)
	}
}

// TestAGeneratedGoalIsIdempotentToo. The replay behaviour comes from
// submitPlan, so it applies to /goals for free — which is what sharing the
// submission path buys.
func TestAGeneratedGoalIsIdempotentToo(t *testing.T) {
	t.Parallel()

	f := newFixture(t, withPlanner(&stubPlanner{result: sampleResult()}))
	headers := map[string]string{"Idempotency-Key": "goal-42"}

	first := f.do(http.MethodPost, "/api/v1/goals", `{"goal":"say hi"}`, headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201: %s", first.Code, first.Body.String())
	}
	second := f.do(http.MethodPost, "/api/v1/goals", `{"goal":"say hi"}`, headers)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status = %d, want 200", second.Code)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("the replay is not marked")
	}

	page, err := f.store.ListJobs(t.Context(), runmesh.JobFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Errorf("%d jobs for one idempotency key", len(page.Jobs))
	}
}
