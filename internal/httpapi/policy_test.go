package httpapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// stubPolicy refuses whatever it is told to and reports a catalogue to match.
type stubPolicy struct {
	denied map[string]string
	reg    tools.Registry
}

func (s stubPolicy) Allows(tool string) error {
	if reason, ok := s.denied[tool]; ok {
		return runmesh.Fatal(runmesh.CodeToolDenied, "%s", reason)
	}
	return nil
}

func (s stubPolicy) Descriptors() []tools.Descriptor {
	out := s.reg.Descriptors()
	for i := range out {
		if reason, ok := s.denied[out[i].Name]; ok {
			out[i].Denied = true
			out[i].DeniedReason = reason
		}
		// The clamped value, standing in for what the real engine resolves.
		out[i].Limits.CPU = "250m"
	}
	return out
}

func withPolicy(p httpapi.Policy) func(*httpapi.Deps) {
	return func(d *httpapi.Deps) { d.Sandbox = p }
}

// TestSubmittingADeniedToolIs400.
//
// The refusal happens at submission, before anything is persisted. The
// alternative — accepting the job and letting a worker fail every step — costs
// a 201, a job id, a queue slot and however long the dispatcher takes to get to
// it, and puts the reason somewhere only an operator with log access can read.
func TestSubmittingADeniedToolIs400(t *testing.T) {
	t.Parallel()

	reg := tools.Builtins(tools.Options{EnableTestTools: true})
	f := newFixture(t, withPolicy(stubPolicy{
		reg:    reg,
		denied: map[string]string{"sleep": "tool \"sleep\" is denied by execution policy"},
	}))

	rec := f.do(http.MethodPost, "/api/v1/jobs", `{
      "name": "denied",
      "steps": [
        {"id": "a", "tool": "echo"},
        {"id": "b", "tool": "sleep", "params": {"seconds": 1}}
      ]
    }`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding the error envelope: %v", err)
	}
	found := false
	for _, d := range env.Error.Details {
		if d.Field == "steps[1].tool" {
			found = true
			if !strings.Contains(d.Issue, runmesh.CodeToolDenied) {
				t.Errorf("issue = %q, want it to carry the stable code %q so a "+
					"client can branch without parsing English", d.Issue, runmesh.CodeToolDenied)
			}
		}
	}
	if !found {
		t.Errorf("no detail names the offending step; details = %+v", env.Error.Details)
	}

	// Nothing was persisted.
	page, err := f.store.ListJobs(t.Context(), runmesh.JobFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Jobs) != 0 {
		t.Errorf("%d jobs were persisted for a plan that was refused", len(page.Jobs))
	}
}

// TestEveryProblemInAPlanIsReportedAtOnce. When the author is a language model
// retrying in a loop, one problem per round trip is the difference between one
// repair and five.
func TestEveryProblemInAPlanIsReportedAtOnce(t *testing.T) {
	t.Parallel()

	reg := tools.Builtins(tools.Options{EnableTestTools: true})
	f := newFixture(t, withPolicy(stubPolicy{
		reg:    reg,
		denied: map[string]string{"sleep": "denied"},
	}))

	// One denied tool AND one malformed parameter, in different steps.
	rec := f.do(http.MethodPost, "/api/v1/jobs", `{
      "name": "two problems",
      "steps": [
        {"id": "a", "tool": "sleep", "params": {"seconds": 1}},
        {"id": "b", "tool": "sleep", "params": {"duration": "not-a-duration"}}
      ]
    }`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding the error envelope: %v", err)
	}
	fields := map[string]bool{}
	for _, d := range env.Error.Details {
		fields[d.Field] = true
	}
	for _, want := range []string{"steps[0].tool", "steps[1].tool", "steps[1].params"} {
		if !fields[want] {
			t.Errorf("no detail for %s; the response reports %v, and a caller "+
				"fixing one problem at a time needs as many round trips as it has "+
				"problems", want, fields)
		}
	}
}

// TestToolsEndpointServesTheEffectiveLimits. Publishing what a descriptor asked
// for tells a planner it has resources the cluster will not give it, and every
// plan built on that number is wrong in the same direction.
func TestToolsEndpointServesTheEffectiveLimits(t *testing.T) {
	t.Parallel()

	reg := tools.Builtins(tools.Options{EnableTestTools: true})
	f := newFixture(t, withPolicy(stubPolicy{
		reg:    reg,
		denied: map[string]string{"fail": "test tools are off in this deployment"},
	}))

	rec := f.do(http.MethodGet, "/api/v1/tools", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Tools []tools.Descriptor `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Tools) == 0 {
		t.Fatal("the catalogue is empty")
	}

	var sawDenied bool
	for _, d := range body.Tools {
		if d.Limits.CPU != "250m" {
			t.Errorf("%s publishes cpu %q, want the policy's resolved value",
				d.Name, d.Limits.CPU)
		}
		if d.Name == "fail" {
			sawDenied = true
			if !d.Denied || d.DeniedReason == "" {
				t.Errorf("the denied tool is published as %+v, want denied with a "+
					"reason: omitting it would make unknown_tool the only evidence "+
					"that it exists but is switched off", d)
			}
		}
	}
	if !sawDenied {
		t.Error("the denied tool is missing from the catalogue entirely")
	}
}
