package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/policy"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// stubSandbox answers whatever the test tells it to.
type stubSandbox struct {
	err    error
	limits tools.Limits
	calls  int
}

func (s *stubSandbox) Limits(_ string, req policy.Request) (tools.Limits, error) {
	s.calls++
	if s.err != nil {
		return tools.Limits{}, s.err
	}
	out := s.limits
	out.Timeout = req.Timeout
	out.MaxAttempts = req.MaxAttempts
	out.MaxOutputBytes = req.MaxOutputBytes
	return out, nil
}

// spyExecutor records whether it was reached at all.
type spyExecutor struct {
	inner tools.Executor
	last  tools.Input
	calls int
}

func (s *spyExecutor) Execute(ctx context.Context, in tools.Input) (tools.Output, error) {
	s.calls++
	s.last = in
	return s.inner.Execute(ctx, in)
}

// TestAPolicyRefusalFailsTheStepOnceAndReachesNoTool.
//
// Two things are being asserted, and the second matters more than the first.
// The step must fail — but it must also fail EXACTLY once, with no retries,
// because a refusal by policy will give the same answer on every attempt.
// Retrying it would spend the step's whole budget re-asking a settled question
// and bury the one message that says which switch to flip.
func TestAPolicyRefusalFailsTheStepOnceAndReachesNoTool(t *testing.T) {
	t.Parallel()

	box := &stubSandbox{err: runmesh.Fatal(runmesh.CodeToolDenied,
		"tool %q is denied by execution policy", "echo")}
	exec := &spyExecutor{inner: tools.Local{Registry: tools.Registry{"echo": tools.Echo{}}}}

	h := newHarness(t, tools.Registry{"echo": tools.Echo{}},
		withSandbox(box),
		withExecutor(exec))

	h.submit("job_denied", []runmesh.PlanStep{{ID: "a", Tool: "echo"}}, runmesh.FailFast)
	h.drainAll()

	job := h.job("job_denied")
	if job.State != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED", job.State)
	}
	step := job.Steps[0]
	if step.Error == nil || step.Error.Code != runmesh.CodeToolDenied {
		t.Fatalf("step error = %+v, want code %q", step.Error, runmesh.CodeToolDenied)
	}
	if step.Error.Retryable {
		t.Error("the refusal was recorded as retryable")
	}
	if step.Attempt != 1 {
		t.Errorf("attempts = %d, want 1: a policy refusal is settled, and asking "+
			"again three times only delays the message that says why", step.Attempt)
	}
	if exec.calls != 0 {
		t.Errorf("the executor ran %d times; a refused step must never reach the "+
			"tool at all", exec.calls)
	}
}

// TestTheResolvedEnvelopeIsWhatTheToolReceives. The engine takes the timeout
// and attempt budget from the step and everything about the sandbox from the
// policy — never from the plan, which from Week 5 a language model writes.
func TestTheResolvedEnvelopeIsWhatTheToolReceives(t *testing.T) {
	t.Parallel()

	box := &stubSandbox{limits: tools.Limits{
		CPU: "250m", Memory: "128Mi", EphemeralStorage: "32Mi",
		Image: "runmesh/python:dev", Network: true,
	}}
	exec := &spyExecutor{inner: tools.Local{Registry: tools.Registry{"echo": tools.Echo{}}}}

	h := newHarness(t, tools.Registry{"echo": tools.Echo{}},
		withSandbox(box),
		withExecutor(exec))

	h.submit("job_env", []runmesh.PlanStep{{ID: "a", Tool: "echo"}}, runmesh.FailFast)
	h.drainAll()

	if exec.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", exec.calls)
	}
	got := exec.last.Limits
	if got.CPU != "250m" || got.Memory != "128Mi" || got.EphemeralStorage != "32Mi" {
		t.Errorf("limits = %+v, want the resolved sandbox", got)
	}
	if got.Image != "runmesh/python:dev" || !got.Network {
		t.Errorf("image/network = %q/%v, want the resolved values: these become "+
			"the pod's image and the label the NetworkPolicy selects on",
			got.Image, got.Network)
	}
	if box.calls != 1 {
		t.Errorf("the policy was consulted %d times for one attempt, want 1", box.calls)
	}
}

// TestThePolicyIsConsultedPerAttempt, not once per submission.
//
// A step can sit behind a retry backoff for minutes and behind a dead worker's
// lease for longer. Resolving the envelope once, at submission, would mean a
// policy tightened in between — a tool denied, the network switched off — binds
// nothing that is already queued, which is precisely the population an operator
// tightening a policy is worried about.
func TestThePolicyIsConsultedPerAttempt(t *testing.T) {
	t.Parallel()

	box := &stubSandbox{}
	h := newHarness(t, tools.Registry{"flaky": flaky{failUntil: 2}},
		withSandbox(box),
		withBackoff(engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}))

	h.submit("job_attempts", []runmesh.PlanStep{
		{ID: "a", Tool: "flaky", MaxAttempts: 3},
	}, runmesh.FailFast)

	// Three attempts, each one past the backoff of the last.
	for i, wait := range []time.Duration{0, time.Second, 2 * time.Second} {
		h.clk.Advance(wait)
		if !h.mustRunOnce() {
			t.Fatalf("attempt %d did not run", i+1)
		}
	}

	if j := h.job("job_attempts"); j.State != runmesh.Succeeded {
		t.Fatalf("job state = %s, want SUCCEEDED after two failures and a success", j.State)
	}
	if box.calls != 3 {
		t.Errorf("the policy was consulted %d times across 3 attempts, want 3", box.calls)
	}
}
