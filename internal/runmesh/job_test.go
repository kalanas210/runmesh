package runmesh_test

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

var testEpoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

func diamond(t *testing.T) *runmesh.Job {
	t.Helper()
	p := runmesh.Plan{Name: "d", Steps: []runmesh.PlanStep{
		{ID: "a", Tool: "echo", Params: json.RawMessage(`{"n":1}`)},
		{ID: "b", Tool: "echo", DependsOn: []string{"a"}},
		{ID: "c", Tool: "echo", DependsOn: []string{"a"}},
		{ID: "d", Tool: "echo", DependsOn: []string{"b", "c"}},
	}}
	return p.Build("job_d", testEpoch, runmesh.Defaults{StepTimeout: time.Second, MaxAttempts: 3})
}

// TestJobCloneIsDeep is the guard on the discipline the entire in-memory store
// rests on. Every read and write clones; one missed field here is a data race
// that only shows up under -race on an exercised path.
func TestJobCloneIsDeep(t *testing.T) {
	t.Parallel()
	original := diamond(t)
	at := testEpoch
	original.CancelRequestedAt = &at
	original.StartedAt = &at
	original.EndedAt = &at
	original.Error = &runmesh.ErrorInfo{Code: "orig", Message: "orig"}
	original.Steps[0].Result = json.RawMessage(`{"ok":true}`)
	original.Steps[0].Error = &runmesh.ErrorInfo{Code: "step", Message: "step"}
	original.Steps[0].ScheduledAt = &at
	original.Steps[0].StartedAt = &at
	original.Steps[0].EndedAt = &at

	clone := original.Clone()

	// Mutate every reachable field on the clone.
	clone.Name = "mutated"
	clone.State = runmesh.Failed
	*clone.CancelRequestedAt = at.Add(time.Hour)
	*clone.StartedAt = at.Add(time.Hour)
	*clone.EndedAt = at.Add(time.Hour)
	clone.Error.Code = "mutated"
	clone.Steps[0].Tool = "mutated"
	clone.Steps[0].Params[2] = 'X'
	clone.Steps[0].Result[2] = 'X'
	clone.Steps[0].Error.Code = "mutated"
	*clone.Steps[0].ScheduledAt = at.Add(time.Hour)
	*clone.Steps[0].StartedAt = at.Add(time.Hour)
	*clone.Steps[0].EndedAt = at.Add(time.Hour)
	clone.Steps[1].DependsOn[0] = "mutated"
	clone.Steps[3] = &runmesh.Step{ID: "replaced"}

	switch {
	case original.Name != "d":
		t.Error("Name is shared")
	case original.State != runmesh.Queued:
		t.Error("State is shared")
	case !original.CancelRequestedAt.Equal(at):
		t.Error("CancelRequestedAt points at shared memory")
	case !original.StartedAt.Equal(at):
		t.Error("StartedAt points at shared memory")
	case !original.EndedAt.Equal(at):
		t.Error("EndedAt points at shared memory")
	case original.Error.Code != "orig":
		t.Error("job Error points at shared memory")
	case original.Steps[0].Tool != "echo":
		t.Error("step Tool is shared")
	case string(original.Steps[0].Params) != `{"n":1}`:
		t.Error("step Params points at shared memory")
	case string(original.Steps[0].Result) != `{"ok":true}`:
		t.Error("step Result points at shared memory")
	case original.Steps[0].Error.Code != "step":
		t.Error("step Error points at shared memory")
	case !original.Steps[0].ScheduledAt.Equal(at):
		t.Error("step ScheduledAt points at shared memory")
	case original.Steps[1].DependsOn[0] != "a":
		t.Error("step DependsOn points at shared memory")
	case original.Steps[3].ID != "d":
		t.Error("the Steps slice itself is shared")
	}
}

func TestCloneOfNilIsNil(t *testing.T) {
	t.Parallel()
	var j *runmesh.Job
	if j.Clone() != nil {
		t.Error("(*Job)(nil).Clone() must be nil")
	}
	var s *runmesh.Step
	if s.Clone() != nil {
		t.Error("(*Step)(nil).Clone() must be nil")
	}
	var e *runmesh.ErrorInfo
	if e.Clone() != nil {
		t.Error("(*ErrorInfo)(nil).Clone() must be nil")
	}
}

// TestBlockedByIsDerived: readiness is a query, not a stored value, so
// blocked_by has to fall to empty the moment the last dependency succeeds,
// with nothing to update.
func TestBlockedByIsDerived(t *testing.T) {
	t.Parallel()
	j := diamond(t)

	if got := j.BlockedBy(j.Step("a")); len(got) != 0 {
		t.Errorf("a is blocked by %v; a root step has no dependencies", got)
	}
	if got := j.BlockedBy(j.Step("d")); len(got) != 2 {
		t.Errorf("d is blocked by %v, want both b and c", got)
	}

	j.Step("b").State = runmesh.Succeeded
	got := j.BlockedBy(j.Step("d"))
	if len(got) != 1 || got[0] != "c" {
		t.Errorf("with b succeeded, d is blocked by %v, want [c]", got)
	}

	// Only SUCCEEDED unblocks. A failed dependency must keep it blocked for
	// ever — which is exactly why markDoomed exists.
	j.Step("c").State = runmesh.Failed
	if got := j.BlockedBy(j.Step("d")); len(got) != 1 || got[0] != "c" {
		t.Errorf("with c FAILED, d is blocked by %v; only SUCCEEDED unblocks", got)
	}

	j.Step("c").State = runmesh.Succeeded
	if got := j.BlockedBy(j.Step("d")); len(got) != 0 {
		t.Errorf("with both dependencies succeeded, d is still blocked by %v", got)
	}
}

func TestQuiescent(t *testing.T) {
	t.Parallel()
	j := diamond(t)
	if j.Quiescent() {
		t.Error("a freshly built job is quiescent")
	}
	for _, s := range j.Steps {
		s.State = runmesh.Succeeded
	}
	if !j.Quiescent() {
		t.Error("a job with only terminal steps is not quiescent")
	}
	j.Steps[2].State = runmesh.Retrying
	if j.Quiescent() {
		t.Error("a job with a RETRYING step is quiescent")
	}
}

// TestNewIDIsSortableAndUnique underwrites keyset pagination: ordering by id
// must equal ordering by creation time, with no tie-breaker column.
func TestNewIDIsSortableAndUnique(t *testing.T) {
	t.Parallel()

	const n = 10_000
	ids := make([]string, 0, n)
	seen := make(map[string]bool, n)
	at := testEpoch

	for i := range n {
		if i%7 == 0 {
			at = at.Add(time.Millisecond) // time advances irregularly
		}
		id := runmesh.NewID("job_", at)
		if seen[id] {
			t.Fatalf("duplicate id %s after %d ids", id, i)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// Ids minted at strictly increasing instants must sort in that order.
	stamped := make([]string, 0, 64)
	base := testEpoch
	for i := range 64 {
		stamped = append(stamped, runmesh.NewID("job_", base.Add(time.Duration(i)*time.Second)))
	}
	sorted := append([]string(nil), stamped...)
	sort.Strings(sorted)
	for i := range stamped {
		if stamped[i] != sorted[i] {
			t.Fatalf("ids do not sort in creation order at index %d:\n got %s\nwant %s",
				i, stamped[i], sorted[i])
		}
	}

	for _, id := range ids[:10] {
		if len(id) != len("job_")+26 {
			t.Fatalf("id %q has length %d; the encoding must be fixed width", id, len(id))
		}
		if id[:4] != "job_" {
			t.Fatalf("id %q lost its prefix", id)
		}
	}
}

func TestAttemptIDAndIdempotencyKey(t *testing.T) {
	t.Parallel()

	// AttemptID names one EXECUTION, so it changes with the attempt.
	a1 := runmesh.AttemptID("job_1", "step_a", 1)
	a2 := runmesh.AttemptID("job_1", "step_a", 2)
	if a1 == a2 {
		t.Error("AttemptID must differ between attempts; it names the execution")
	}
	if a1 != "job_1.step_a.1" {
		t.Errorf("AttemptID = %q, want a readable job.step.attempt form", a1)
	}

	// IdempotencyKey names the STEP, so it is stable across attempts — that
	// stability is the whole reason retrying a side-effecting tool is safe.
	k1 := runmesh.IdempotencyKey("job_1", "step_a")
	k2 := runmesh.IdempotencyKey("job_1", "step_a")
	if k1 != k2 {
		t.Error("IdempotencyKey must be stable across attempts of the same step")
	}
	if k1 == runmesh.IdempotencyKey("job_1", "step_b") {
		t.Error("IdempotencyKey collides across steps of one job")
	}
	if k1 == runmesh.IdempotencyKey("job_2", "step_a") {
		t.Error("IdempotencyKey collides across jobs")
	}
	// The separator must make ("ab","c") and ("a","bc") distinct.
	if runmesh.IdempotencyKey("ab", "c") == runmesh.IdempotencyKey("a", "bc") {
		t.Error("IdempotencyKey is ambiguous across a job/step boundary")
	}
}

func TestFailurePolicyJSON(t *testing.T) {
	t.Parallel()
	for wire, want := range map[string]runmesh.FailurePolicy{
		`"fail_fast"`:           runmesh.FailFast,
		`"continue_on_failure"`: runmesh.ContinueOnFailure,
		`""`:                    runmesh.FailFast, // the safe default
	} {
		var got runmesh.FailurePolicy
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatalf("unmarshal %s: %v", wire, err)
		}
		if got != want {
			t.Errorf("%s decoded to %s, want %s", wire, got, want)
		}
	}
	var bad runmesh.FailurePolicy
	if err := json.Unmarshal([]byte(`"keep_going_probably"`), &bad); err == nil {
		t.Error("an unknown failure policy was accepted")
	}
}

func TestToolErrorClassification(t *testing.T) {
	t.Parallel()
	retry := runmesh.Retry("rate_limited", "slow down: %d", 429)
	if !retry.Retryable || retry.Code != "rate_limited" {
		t.Errorf("Retry produced %+v", retry)
	}
	if retry.Error() != "rate_limited: slow down: 429" {
		t.Errorf("Error() = %q", retry.Error())
	}

	fatal := runmesh.Fatal("bad_input", "field %s", "x")
	if fatal.Retryable {
		t.Error("Fatal produced a retryable error")
	}

	in := runmesh.RetryIn(5*time.Second, "backoff", "wait")
	if !in.Retryable || in.RetryAfter != 5*time.Second {
		t.Errorf("RetryIn produced %+v", in)
	}
}
