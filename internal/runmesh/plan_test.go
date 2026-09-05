package runmesh_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

var testLimits = runmesh.Limits{
	MaxSteps:       10,
	MaxDependsOn:   3,
	MaxParamsBytes: 64,
	MaxStepTimeout: time.Minute,
	MaxAttempts:    5,
	MaxResultBytes: 1024,
}

func known(tool string) bool { return tool == "echo" || tool == "sleep" }

func step(id string, deps ...string) runmesh.PlanStep {
	return runmesh.PlanStep{ID: id, Tool: "echo", DependsOn: deps}
}

// TestPlanValidate pins the exact field paths the 400 body reports. A caller
// fixing a rejected plan — and in Week 5 that caller is an LLM in a retry loop
// — needs to be told precisely which element is wrong, not merely that
// something is.
func TestPlanValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		plan  runmesh.Plan
		valid bool
		// wantFields are field paths that MUST appear in the failure detail.
		wantFields []string
		wantIssues []string
	}{
		{
			name:  "diamond is valid",
			plan:  runmesh.Plan{Name: "ok", Steps: []runmesh.PlanStep{step("a"), step("b", "a"), step("c", "a"), step("d", "b", "c")}},
			valid: true,
		},
		{
			name:  "forest of independent steps is valid",
			plan:  runmesh.Plan{Name: "ok", Steps: []runmesh.PlanStep{step("a"), step("b"), step("c")}},
			valid: true,
		},
		{
			name:       "name is required",
			plan:       runmesh.Plan{Steps: []runmesh.PlanStep{step("a")}},
			wantFields: []string{"name"},
			wantIssues: []string{"required"},
		},
		{
			name:       "steps are required",
			plan:       runmesh.Plan{Name: "x"},
			wantFields: []string{"steps"},
			wantIssues: []string{"required"},
		},
		{
			name: "too many steps",
			plan: runmesh.Plan{Name: "x", Steps: func() []runmesh.PlanStep {
				out := make([]runmesh.PlanStep, 11)
				for i := range out {
					out[i] = step("s" + string(rune('a'+i)))
				}
				return out
			}()},
			wantFields: []string{"steps"},
			wantIssues: []string{"too_many"},
		},
		{
			name:       "malformed step id",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("Bad Id!")}},
			wantFields: []string{"steps[0].id"},
			wantIssues: []string{"malformed"},
		},
		{
			name:       "duplicate step id",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a"), step("a")}},
			wantFields: []string{"steps[1].id"},
			wantIssues: []string{"duplicate"},
		},
		{
			name:       "unknown tool",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "shell_execute"}}},
			wantFields: []string{"steps[0].tool"},
			wantIssues: []string{"unknown_tool"},
		},
		{
			name:       "dangling dependency",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a", "ghost")}},
			wantFields: []string{"steps[0].depends_on[0]"},
			wantIssues: []string{"unknown_step"},
		},
		{
			name:       "self dependency",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a", "a")}},
			wantFields: []string{"steps[0].depends_on[0]"},
			wantIssues: []string{"self_dependency"},
		},
		{
			name:       "duplicate dependency",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a"), step("b", "a", "a")}},
			wantFields: []string{"steps[1].depends_on[1]"},
			wantIssues: []string{"duplicate"},
		},
		{
			name:       "too many dependencies",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a"), step("b"), step("c"), step("d"), step("e", "a", "b", "c", "d")}},
			wantFields: []string{"steps[4].depends_on"},
			wantIssues: []string{"too_many"},
		},
		{
			name: "oversized params",
			plan: runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{
				ID: "a", Tool: "echo",
				Params: json.RawMessage(`{"blob":"` + strings.Repeat("x", 100) + `"}`),
			}}},
			wantFields: []string{"steps[0].params"},
			wantIssues: []string{"too_large"},
		},
		{
			name: "invalid params json",
			plan: runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{
				ID: "a", Tool: "echo", Params: json.RawMessage(`{not json`),
			}}},
			wantFields: []string{"steps[0].params"},
			wantIssues: []string{"invalid_json"},
		},
		{
			name:       "timeout above the ceiling",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo", TimeoutSec: 3600}}},
			wantFields: []string{"steps[0].timeout_seconds"},
			wantIssues: []string{"out_of_range"},
		},
		{
			name:       "negative timeout",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo", TimeoutSec: -1}}},
			wantFields: []string{"steps[0].timeout_seconds"},
			wantIssues: []string{"out_of_range"},
		},
		{
			// time.Duration(n) * time.Second overflows int64 above roughly 9.2e9
			// and wraps NEGATIVE, which compares as comfortably under any
			// ceiling. A step built from it gets a deadline already in the past.
			name:       "timeout_seconds that overflows a Duration",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo", TimeoutSec: 10_000_000_000}}},
			wantFields: []string{"steps[0].timeout_seconds"},
			wantIssues: []string{"out_of_range"},
		},
		{
			name:       "timeout_seconds at the far edge of int64",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo", TimeoutSec: 9223372036854775807}}},
			wantFields: []string{"steps[0].timeout_seconds"},
			wantIssues: []string{"out_of_range"},
		},
		{
			name:       "max_attempts above the ceiling",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{{ID: "a", Tool: "echo", MaxAttempts: 99}}},
			wantFields: []string{"steps[0].max_attempts"},
			wantIssues: []string{"out_of_range"},
		},
		{
			name:       "two-step cycle",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a", "b"), step("b", "a")}},
			wantFields: []string{"steps"},
			wantIssues: []string{"cycle:a,b"},
		},
		{
			name:       "four-step cycle",
			plan:       runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{step("a", "d"), step("b", "a"), step("c", "b"), step("d", "c")}},
			wantFields: []string{"steps"},
			wantIssues: []string{"cycle:a,b,c,d"},
		},
		{
			name: "cycle in one component of a forest",
			plan: runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{
				step("ok1"), step("ok2", "ok1"),
				step("bad1", "bad2"), step("bad2", "bad1"),
			}},
			wantFields: []string{"steps"},
			// Only the stuck component is named; the healthy one is not.
			wantIssues: []string{"cycle:bad1,bad2"},
		},
		{
			name: "every problem is reported at once",
			plan: runmesh.Plan{Steps: []runmesh.PlanStep{
				{ID: "BAD", Tool: "nope"},
				{ID: "b", Tool: "echo", DependsOn: []string{"ghost"}},
			}},
			wantFields: []string{"name", "steps[0].id", "steps[0].tool", "steps[1].depends_on[0]"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.plan.Validate(known, testLimits)

			if tc.valid {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid plan accepted")
			}
			var ve *runmesh.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error is %T, want *runmesh.ValidationError", err)
			}

			fields := map[string]string{}
			for _, d := range ve.Details {
				fields[d.Field] = d.Issue
			}
			for i, want := range tc.wantFields {
				issue, ok := fields[want]
				if !ok {
					t.Errorf("no detail for field %q; got %+v", want, ve.Details)
					continue
				}
				if i < len(tc.wantIssues) && issue != tc.wantIssues[i] {
					t.Errorf("field %q issue = %q, want %q", want, issue, tc.wantIssues[i])
				}
			}
		})
	}
}

// TestFindCycleIsDeterministic matters because the 400 body is pinned by the
// tests above: a cycle report that varied run to run would make them flaky.
func TestFindCycleIsDeterministic(t *testing.T) {
	t.Parallel()
	p := runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{
		step("a", "c"), step("b", "a"), step("c", "b"), step("d"), step("e", "d"),
	}}

	var first string
	for i := range 100 {
		err := p.Validate(known, testLimits)
		if err == nil {
			t.Fatal("cyclic plan accepted")
		}
		var ve *runmesh.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("error is %T", err)
		}
		var got string
		for _, d := range ve.Details {
			if strings.HasPrefix(d.Issue, "cycle:") {
				got = d.Issue
			}
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d reported %q, run 0 reported %q; cycle detection must be deterministic", i, got, first)
		}
	}
	if first != "cycle:a,b,c" {
		t.Fatalf("cycle = %q, want the three stuck steps in declaration order", first)
	}
}

func TestPlanBuildIsPure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	defaults := runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3}

	p := runmesh.Plan{
		Name:          "build",
		Priority:      7,
		OnStepFailure: runmesh.ContinueOnFailure,
		Steps: []runmesh.PlanStep{
			{ID: "a", Tool: "echo", Params: json.RawMessage(`{"x":1}`)},
			{ID: "b", Tool: "sleep", DependsOn: []string{"a"}, TimeoutSec: 5, MaxAttempts: 1},
		},
	}

	j := p.Build("job_1", now, defaults)
	if j.ID != "job_1" || j.State != runmesh.Queued || j.Version != 1 {
		t.Fatalf("job header = %+v", j)
	}
	if j.Priority != 7 || j.OnStepFailure != runmesh.ContinueOnFailure {
		t.Errorf("plan settings were not carried onto the job: %+v", j)
	}
	if !j.CreatedAt.Equal(now) || !j.UpdatedAt.Equal(now) {
		t.Errorf("timestamps = %v / %v, want the supplied now", j.CreatedAt, j.UpdatedAt)
	}

	a, b := j.Steps[0], j.Steps[1]
	if a.Timeout != defaults.StepTimeout || a.MaxAttempts != defaults.MaxAttempts {
		t.Errorf("step a did not take the defaults: timeout=%v attempts=%d", a.Timeout, a.MaxAttempts)
	}
	if b.Timeout != 5*time.Second || b.MaxAttempts != 1 {
		t.Errorf("step b did not take its overrides: timeout=%v attempts=%d", b.Timeout, b.MaxAttempts)
	}
	for _, s := range j.Steps {
		if s.State != runmesh.Queued {
			t.Errorf("step %s starts in %s; every step starts QUEUED regardless of depth", s.ID, s.State)
		}
		if !s.NextAttemptAt.Equal(now) {
			t.Errorf("step %s next_attempt_at = %v, want now", s.ID, s.NextAttemptAt)
		}
	}

	// Build must not alias the plan's slices.
	p.Steps[0].Params[2] = 'X'
	p.Steps[1].DependsOn[0] = "mutated"
	if string(j.Steps[0].Params) != `{"x":1}` {
		t.Error("Build aliased the plan's params")
	}
	if j.Steps[1].DependsOn[0] != "a" {
		t.Error("Build aliased the plan's depends_on")
	}
}

// TestBuildNeverInstallsANegativeDeadline is the regression for an overflow an
// adversarial review found.
//
// Build is exported, so a caller that skipped Validate must still not be able
// to produce a step whose context is expired the moment it is created — which
// would time out every attempt instantly until the retry budget was gone.
func TestBuildNeverInstallsANegativeDeadline(t *testing.T) {
	t.Parallel()
	defaults := runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3}

	for _, sec := range []int{
		10_000_000_000,      // overflows int64 nanoseconds
		9223372036854775807, // math.MaxInt64
		1 << 62,
	} {
		p := runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{
			{ID: "a", Tool: "echo", TimeoutSec: sec},
		}}
		// Validation rejects it...
		if err := p.Validate(known, testLimits); err == nil {
			t.Errorf("Validate accepted timeout_seconds=%d", sec)
		}
		// ...and Build refuses to install it even if validation was skipped.
		got := p.Build("job_1", testEpoch, defaults).Steps[0].Timeout
		if got <= 0 {
			t.Errorf("Build with timeout_seconds=%d produced a %s deadline; a step "+
				"would be expired before it started", sec, got)
		}
		if got != defaults.StepTimeout {
			t.Errorf("Build with an out-of-range timeout_seconds=%d used %s, want the default %s",
				sec, got, defaults.StepTimeout)
		}
	}

	// A large but legitimate timeout is still honoured.
	p := runmesh.Plan{Name: "x", Steps: []runmesh.PlanStep{
		{ID: "a", Tool: "echo", TimeoutSec: 3600},
	}}
	if got := p.Build("job_1", testEpoch, defaults).Steps[0].Timeout; got != time.Hour {
		t.Errorf("a one-hour timeout became %s", got)
	}
}
