package jobstate_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

// These functions are the reason both stores agree, so they are tested directly
// as well as through both stores. They are pure — no clock, no lock, no IO — so
// a table walks cases here that would each cost a store, a transaction and a
// fabricated timeline to reach from outside.

func step(id string, state runmesh.State, deps ...string) *runmesh.Step {
	s := &runmesh.Step{
		ID: id, Tool: "echo", State: state,
		MaxAttempts: 3, NextAttemptAt: epoch, Version: 1,
	}
	if len(deps) > 0 {
		s.DependsOn = deps
	}
	if state.Terminal() {
		ended := epoch
		s.EndedAt = &ended
	}
	return s
}

func job(steps ...*runmesh.Step) *runmesh.Job {
	return &runmesh.Job{
		ID: "job_a", Name: "j", State: runmesh.Queued,
		CreatedAt: epoch, UpdatedAt: epoch, Version: 1,
		Steps: steps,
	}
}

// TestRollupPrecedence walks the whole precedence rule.
//
// The order is not arbitrary and each rule earns its place: an operator's
// cancel outranks everything because their intent is the more useful answer
// even if a step failed on its way out; TIMED_OUT outranks FAILED so a
// fail-fast job reports the failing step's own state, which is what makes
// TIMED_OUT meaningful at job level without a job-level deadline existing.
func TestRollupPrecedence(t *testing.T) {
	t.Parallel()

	started := epoch
	cancelled := epoch

	cases := []struct {
		name string
		job  *runmesh.Job
		want runmesh.State
	}{
		{"nothing has run yet", job(step("a", runmesh.Queued)), runmesh.Queued},
		{"a claimed step makes the job RUNNING", func() *runmesh.Job {
			j := job(step("a", runmesh.Scheduled))
			j.Steps[0].ScheduledAt = &started
			return j
		}(), runmesh.Running},
		{"a retrying step is still outstanding", func() *runmesh.Job {
			j := job(step("a", runmesh.Retrying), step("b", runmesh.Succeeded))
			j.Steps[0].ScheduledAt = &started
			return j
		}(), runmesh.Running},
		{"all succeeded", job(step("a", runmesh.Succeeded), step("b", runmesh.Succeeded)),
			runmesh.Succeeded},
		{"timed out outranks failed",
			job(step("a", runmesh.Failed), step("b", runmesh.TimedOut)), runmesh.TimedOut},
		{"failed outranks cancelled",
			job(step("a", runmesh.Cancelled), step("b", runmesh.Failed)), runmesh.Failed},
		{"cancelled outranks succeeded",
			job(step("a", runmesh.Succeeded), step("b", runmesh.Cancelled)), runmesh.Cancelled},
		{"an operator cancel outranks a step that failed on the way out", func() *runmesh.Job {
			j := job(step("a", runmesh.Failed))
			j.CancelRequestedAt = &cancelled
			j.CancelReason = runmesh.CancelUser
			return j
		}(), runmesh.Cancelled},
		{"a fail-fast cancel does NOT outrank the failure that caused it", func() *runmesh.Job {
			j := job(step("a", runmesh.Failed), step("b", runmesh.Cancelled))
			j.CancelRequestedAt = &cancelled
			j.CancelReason = runmesh.CancelStepFailed
			return j
		}(), runmesh.Failed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := jobstate.Rollup(tc.job)
			if got != tc.want {
				t.Fatalf("Rollup = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestMarkDoomedIsTransitive: the sweep has to follow the DAG, not just one
// hop. Without it, continue_on_failure hangs — nine analyses finish, the tenth
// fails, and the report that depends on all ten waits for a dependency that
// will never succeed.
func TestMarkDoomedIsTransitive(t *testing.T) {
	t.Parallel()

	// a(FAILED) -> b -> c, and an independent d that must be left alone.
	j := job(
		step("a", runmesh.Failed),
		step("b", runmesh.Queued, "a"),
		step("c", runmesh.Queued, "b"),
		step("d", runmesh.Queued),
	)

	changed := jobstate.MarkDoomed(j, epoch)

	var ids []string
	for _, s := range changed {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"b", "c"}) {
		t.Fatalf("MarkDoomed changed %v, want [b c]", ids)
	}
	for _, id := range []string{"b", "c"} {
		s := j.Step(id)
		if s.State != runmesh.Cancelled {
			t.Errorf("step %s = %s, want CANCELLED", id, s.State)
		}
		if s.Error == nil || s.Error.Code != runmesh.CodeDepFailed {
			t.Errorf("step %s carries error %+v, want code %s", id, s.Error, runmesh.CodeDepFailed)
		}
	}
	if j.Step("d").State != runmesh.Queued {
		t.Errorf("independent step d = %s; a doomed branch must not touch it", j.Step("d").State)
	}
}

// TestMarkDoomedLeavesLeasedStepsAlone: a worker owns an Active step and will
// write its own outcome. Reaching in here would be exactly the zombie-overwrite
// the fencing token exists to prevent — and it would be this code doing it.
func TestMarkDoomedLeavesLeasedStepsAlone(t *testing.T) {
	t.Parallel()

	running := step("b", runmesh.Running, "a")
	running.LeaseID = "lse_held"
	j := job(step("a", runmesh.Failed), running)

	if changed := jobstate.MarkDoomed(j, epoch); len(changed) != 0 {
		t.Fatalf("MarkDoomed touched %d leased steps, want 0", len(changed))
	}
	if running.State != runmesh.Running || running.LeaseID != "lse_held" {
		t.Errorf("a leased step was modified: state %s, lease %q", running.State, running.LeaseID)
	}
}

// TestMarkDoomedTerminatesOnACycle. Plan validation rejects cycles, so this
// input cannot reach a store — which is why the BFS carries a seen set rather
// than trusting that. A policy function that hangs on malformed input is a
// worse failure than one that returns nothing useful.
func TestMarkDoomedTerminatesOnACycle(t *testing.T) {
	t.Parallel()

	j := job(
		step("a", runmesh.Failed),
		step("b", runmesh.Queued, "a", "c"),
		step("c", runmesh.Queued, "b"),
	)
	// Reaching the assertion at all is the assertion: an unbounded walk would
	// hang here and fail on the test binary's timeout.
	if got := len(jobstate.MarkDoomed(j, epoch)); got != 2 {
		t.Fatalf("MarkDoomed changed %d steps, want 2", got)
	}
}

// TestClaimableGates walks the readiness predicate one clause at a time. It is
// the Go twin of the SKIP LOCKED query, so every gate here is a WHERE clause
// there, and a divergence is a step that runs when it should not.
func TestClaimableGates(t *testing.T) {
	t.Parallel()

	cancelled := epoch
	later := epoch.Add(time.Minute)

	cases := []struct {
		name  string
		build func() (*runmesh.Job, *runmesh.Step)
		want  bool
	}{
		{"a queued step with no dependencies", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Queued))
			return j, j.Steps[0]
		}, true},
		{"a retrying step whose backoff has elapsed", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Retrying))
			return j, j.Steps[0]
		}, true},
		{"a retrying step still inside its backoff", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Retrying))
			j.Steps[0].NextAttemptAt = later
			return j, j.Steps[0]
		}, false},
		{"a step whose lease has not expired", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Queued))
			j.Steps[0].LeaseExpiresAt = later
			return j, j.Steps[0]
		}, false},
		{"a step on a cancel-flagged job", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Queued))
			j.CancelRequestedAt = &cancelled
			return j, j.Steps[0]
		}, false},
		{"a step on a terminal job", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Queued))
			j.State = runmesh.Failed
			return j, j.Steps[0]
		}, false},
		{"a dependency that has not succeeded", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Running), step("b", runmesh.Queued, "a"))
			return j, j.Steps[1]
		}, false},
		{"every dependency succeeded", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("a", runmesh.Succeeded), step("b", runmesh.Queued, "a"))
			return j, j.Steps[1]
		}, true},
		// The clause the SQL spells doubly-negated. A dependency naming a step
		// that does not exist must BLOCK, not vanish: plan validation makes it
		// unreachable today, and "unreachable because of a check somewhere
		// else" is the assumption that stops being true later.
		{"a dependency on a step that does not exist", func() (*runmesh.Job, *runmesh.Step) {
			j := job(step("b", runmesh.Queued, "ghost"))
			return j, j.Steps[0]
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, s := tc.build()
			if got := jobstate.Claimable(j, s, epoch, nil); got != tc.want {
				t.Fatalf("Claimable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaimableToolFilter(t *testing.T) {
	t.Parallel()

	j := job(step("a", runmesh.Queued))
	s := j.Steps[0] // tool "echo"

	if !jobstate.Claimable(j, s, epoch, nil) {
		t.Error("a nil tool filter must mean any tool")
	}
	if !jobstate.Claimable(j, s, epoch, map[string]bool{"echo": true}) {
		t.Error("a matching tool filter refused the step")
	}
	if jobstate.Claimable(j, s, epoch, map[string]bool{"sleep": true}) {
		t.Error("a non-matching tool filter claimed the step; Week 3 splits fleets on this")
	}
}

// TestReconcileRaisesFailFastFromAnyWriter is the regression an adversarial
// review found in Week 1, held here at the level it was actually fixed at.
//
// The trigger used to live in the store's Finish path, so a step driven to
// FAILED by the lease sweep instead — a dead worker exhausting its budget —
// raised no cancel flag, its dependents sat QUEUED for ever, and the job never
// terminalised. It is a property of the transition now, so every writer in
// every store inherits it, including ones not written yet.
func TestReconcileRaisesFailFastFromAnyWriter(t *testing.T) {
	t.Parallel()

	j := job(step("a", runmesh.Failed), step("b", runmesh.Queued, "a"))
	j.State = runmesh.Running
	started := epoch
	j.StartedAt = &started

	var events []runmesh.Event
	jobstate.Reconcile(j, epoch, func(e runmesh.Event) { events = append(events, e) })

	if j.CancelRequestedAt == nil || j.CancelReason != runmesh.CancelStepFailed {
		t.Fatalf("no fail-fast cancel was raised: at=%v reason=%q",
			j.CancelRequestedAt, j.CancelReason)
	}
	if st := j.Step("b").State; st != runmesh.Cancelled {
		t.Errorf("dependent b = %s, want CANCELLED", st)
	}
	if j.State != runmesh.Failed {
		t.Errorf("job = %s, want FAILED: the failing step's state wins, not the "+
			"cancelled sibling's", j.State)
	}

	var types []runmesh.EventType
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []runmesh.EventType{
		runmesh.JobCancelRequested, runmesh.StepFinished, runmesh.JobFinished,
	}
	if !slices.Equal(types, want) {
		t.Errorf("events = %v, want %v", types, want)
	}
}

// TestReconcileIsIdempotent: every store method ends in Reconcile, and several
// of them call it on a job nothing changed. A second pass must not re-cancel,
// re-emit or re-terminalise anything.
func TestReconcileIsIdempotent(t *testing.T) {
	t.Parallel()

	j := job(step("a", runmesh.Succeeded))
	started := epoch
	j.State, j.StartedAt = runmesh.Running, &started

	var first, second int
	jobstate.Reconcile(j, epoch, func(runmesh.Event) { first++ })
	jobstate.Reconcile(j, epoch, func(runmesh.Event) { second++ })

	if first == 0 {
		t.Fatal("the first reconcile emitted nothing; the job should have finished")
	}
	if second != 0 {
		t.Errorf("a second reconcile emitted %d more events; it must be a no-op", second)
	}
	if j.State != runmesh.Succeeded {
		t.Errorf("job = %s after two reconciles, want SUCCEEDED", j.State)
	}
}
