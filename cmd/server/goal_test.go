package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The Week-5 acceptance test: a sentence in, a finished job out.
//
//	goal ──▶ planner ──▶ validated plan ──▶ scheduler ──▶ DAG ──▶ report
//
// It runs the whole binary over real HTTP with the heuristic planner, which is
// exactly why that planner exists: this path has to be exercisable with no API
// key, no billing account and no network, and it has to be deterministic enough
// to assert on rather than sample.

type planView struct {
	Name  string `json:"name"`
	Steps []struct {
		ID        string          `json:"id"`
		Tool      string          `json:"tool"`
		Params    json.RawMessage `json:"params"`
		DependsOn []string        `json:"depends_on"`
	} `json:"steps"`
}

type traceView struct {
	Model     string   `json:"model"`
	Reasoning string   `json:"reasoning"`
	Tools     []string `json:"tools_offered"`
	Attempts  []struct {
		N        int  `json:"n"`
		Accepted bool `json:"accepted"`
	} `json:"attempts"`
}

// TestEndToEndGoalToFinishedJob.
func TestEndToEndGoalToFinishedJob(t *testing.T) {
	baseURL, _ := boot(t, map[string]string{
		"RUNMESH_PLANNER": "heuristic",
	})

	// No URLs, so no fetch and no network: the plan degrades to something the
	// local executor can genuinely run end to end.
	status, body := request(t, http.MethodPost, baseURL+"/api/v1/goals",
		`{"goal":"summarise what this runtime does","name":"week five"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("POST /goals = %d, want 201. body: %s", status, body)
	}

	var created struct {
		Job   jobView   `json:"job"`
		Plan  planView  `json:"plan"`
		Trace traceView `json:"trace"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	if created.Job.ID == "" {
		t.Fatalf("no job was created: %s", body)
	}
	// The plan and its provenance came back with the job. A generated plan that
	// executes without the caller being able to see what it was, and why, is
	// the thing this whole week exists to avoid.
	if len(created.Plan.Steps) == 0 {
		t.Fatalf("no plan in the response: %s", body)
	}
	if created.Trace.Model != "heuristic" {
		t.Errorf("trace model = %q, want heuristic", created.Trace.Model)
	}
	if created.Trace.Reasoning == "" {
		t.Error("the trace carries no reasoning")
	}
	if len(created.Trace.Tools) == 0 {
		t.Error("the trace does not record which tools were on offer; a plan is " +
			"only explicable next to the choices that were available")
	}

	job := waitForJob(t, baseURL, created.Job.ID)
	if job.State != "SUCCEEDED" {
		t.Fatalf("job state = %s, want SUCCEEDED. steps: %+v", job.State, job.Steps)
	}
	for _, s := range job.Steps {
		if s.State != "SUCCEEDED" {
			t.Errorf("step %s = %s (%+v)", s.ID, s.State, s.Error)
		}
	}
}

// TestEndToEndGoalProducesARealDAGAndAReport.
//
// URLs in the goal become concurrent fetches joined by a report. The fetches
// will FAIL here — there is no network in a test and http_request is denied by
// default — so the assertion is about the plan's SHAPE, which is what the
// planner is responsible for, rather than about the execution, which is not.
func TestEndToEndGoalProducesARealDAG(t *testing.T) {
	baseURL, _ := boot(t, map[string]string{
		"RUNMESH_PLANNER": "heuristic",
	})

	status, body := request(t, http.MethodPost, baseURL+"/api/v1/plans",
		`{"goal":"compare https://a.example/x.csv with https://b.example/y.csv and report"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("POST /plans = %d, want 200. body: %s", status, body)
	}

	var got struct {
		Plan  planView  `json:"plan"`
		Trace traceView `json:"trace"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := map[string][]string{}
	tools := map[string]string{}
	for _, s := range got.Plan.Steps {
		byID[s.ID] = s.DependsOn
		tools[s.ID] = s.Tool
	}

	// http_request is denied in this deployment — RUNMESH_POLICY_ALLOW_NETWORK
	// is false by default — so the planner must not have used it. That is the
	// execution policy reaching all the way back into planning.
	for id, tool := range tools {
		if tool == "http_request" {
			t.Errorf("step %s uses http_request, which policy denies here", id)
		}
	}
	if _, ok := byID["report"]; !ok {
		t.Fatalf("no report step; the plan is %+v", got.Plan.Steps)
	}
	if !strings.Contains(got.Trace.Reasoning, "http_request") {
		t.Errorf("the reasoning does not explain why nothing was fetched: %q",
			got.Trace.Reasoning)
	}
}

// TestGoalsWithNoPlannerIs501. The default is "none", so a deployment that
// never configured a planner says so rather than quietly planning with rules
// over keywords.
func TestGoalsWithNoPlannerIs501(t *testing.T) {
	baseURL, _ := boot(t, nil)

	status, body := request(t, http.MethodPost, baseURL+"/api/v1/goals",
		`{"goal":"anything"}`, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("POST /goals = %d, want 501. body: %s", status, body)
	}
	if !strings.Contains(string(body), "RUNMESH_PLANNER") {
		t.Errorf("the 501 does not name the variable to set: %s", body)
	}
}

// TestGeminiPlannerRefusesToStartWithoutAKey. A boot failure naming the
// variable, not a 500 per planning request.
func TestGeminiPlannerRefusesToStartWithoutAKey(t *testing.T) {
	env := map[string]string{
		"RUNMESH_HTTP_ADDR": "127.0.0.1:0",
		"RUNMESH_API_KEYS":  "ci=" + testKey,
		"RUNMESH_PLANNER":   "gemini",
	}
	var out strings.Builder
	code := run(t.Context(), nil, func(k string) string { return env[k] }, &out, &out)
	if code == 0 {
		t.Fatal("the server started with RUNMESH_PLANNER=gemini and no API key")
	}
	if !strings.Contains(out.String(), "RUNMESH_GEMINI_API_KEY") {
		t.Errorf("the error does not name the missing variable:\n%s", out.String())
	}
}
