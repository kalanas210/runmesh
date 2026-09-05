package runmesh

import (
	"encoding/json"
	"fmt"
	"time"
)

// FailurePolicy decides what happens to the rest of a job when one step fails
// terminally.
type FailurePolicy uint8

const (
	// FailFast stops the job at the first terminal step failure. The default,
	// because a report step that runs on missing input produces a wrong answer
	// rather than no answer.
	FailFast FailurePolicy = iota
	// ContinueOnFailure runs everything still runnable and rolls the job up at
	// the end. Plan §4's first use case — ten independent CSV analyses where
	// one bad file must not lose the other nine — needs this.
	ContinueOnFailure
)

func (p FailurePolicy) String() string {
	if p == ContinueOnFailure {
		return "continue_on_failure"
	}
	return "fail_fast"
}

func (p FailurePolicy) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

func (p *FailurePolicy) UnmarshalText(b []byte) error {
	switch string(b) {
	case "", "fail_fast":
		*p = FailFast
	case "continue_on_failure":
		*p = ContinueOnFailure
	default:
		return fmt.Errorf("runmesh: unknown failure policy %q", b)
	}
	return nil
}

// CancelReason distinguishes the two triggers that share one cancellation
// mechanism. Keeping them one mechanism means fail-fast propagation is not a
// second, separately-buggy code path.
type CancelReason string

const (
	CancelUser       CancelReason = "user"
	CancelStepFailed CancelReason = "step_failed"
)

// Job maps 1:1 onto the Week-2 jobs row.
type Job struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	State         State         `json:"state"`
	Priority      int           `json:"priority"`
	OnStepFailure FailurePolicy `json:"on_step_failure"`
	Steps         []*Step       `json:"steps"`

	// IdempotencyKey is UNIQUE in Week 2 and never appears on the wire.
	IdempotencyKey string `json:"-"`

	// CancelRequestedAt is a FLAG, not a state. The job stays RUNNING while
	// in-flight steps drain; only the rollup moves it to a terminal state.
	// This is what stops the store from claiming a job is CANCELLED while a
	// worker is still writing to one of its steps.
	CancelRequestedAt *time.Time   `json:"cancel_requested_at,omitempty"`
	CancelReason      CancelReason `json:"cancel_reason,omitempty"`

	Error *ErrorInfo `json:"error,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// Version is bumped on every mutation. Week 1 does not need it for
	// correctness (the fencing token subsumes it), but it ships from the start
	// because the API returns it as an ETag and adding a column later is a
	// migration plus a backfill plus every INSERT in the codebase.
	Version uint64 `json:"version"`
	// EventSeq is the per-job event counter. Week 2 bumps it inside the same
	// UPDATE that writes the transition, which is what keeps Seq gap-free
	// without a lost-update race between two concurrent appends.
	EventSeq uint64 `json:"-"`
}

// Step maps 1:1 onto the Week-2 job_steps row, PRIMARY KEY (job_id, id).
type Step struct {
	ID        string          `json:"id"` // unique within the job, author-supplied
	Tool      string          `json:"tool"`
	Params    json.RawMessage `json:"params,omitempty"`
	DependsOn []string        `json:"depends_on,omitempty"`

	State       State         `json:"state"`
	Timeout     time.Duration `json:"-"` // wire form is timeout_seconds, in the DTO
	MaxAttempts int           `json:"max_attempts"`

	// Attempt is MONOTONIC: it increments on every claim and is never
	// decremented. It names the execution — it becomes the AttemptID and, in
	// Week 3, the Kubernetes Job name.
	//
	// Failures is the retry BUDGET and increments only when the classifier
	// says a real failure occurred. Conflating the two is how a rolling
	// restart silently exhausts every in-flight job's retries.
	Attempt  int `json:"attempt"`
	Failures int `json:"failures"`

	// NextAttemptAt is the backoff gate, and it is the entire retry timer.
	// There is no sleeping goroutine per retrying step: a step in RETRYING is
	// simply not yet claimable.
	NextAttemptAt time.Time `json:"-"`

	LeaseID        string    `json:"-"` // fencing token, regenerated on every claim
	LeaseOwner     string    `json:"-"`
	LeaseExpiresAt time.Time `json:"-"`

	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorInfo      `json:"error,omitempty"`

	ScheduledAt *time.Time `json:"scheduled_at,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	Version     uint64     `json:"version"`
}

// Step returns the named step, or nil.
func (j *Job) Step(id string) *Step {
	for _, s := range j.Steps {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// BlockedBy is DERIVED on read: the dependencies that are not yet SUCCEEDED.
// Nothing stores it, so nothing can disagree with the claim predicate.
func (j *Job) BlockedBy(s *Step) []string {
	var out []string
	for _, d := range s.DependsOn {
		if p := j.Step(d); p == nil || p.State != Succeeded {
			out = append(out, d)
		}
	}
	return out
}

// Quiescent reports that no step can make further progress on its own.
func (j *Job) Quiescent() bool {
	for _, s := range j.Steps {
		if !s.State.Terminal() {
			return false
		}
	}
	return true
}

// Clone is a full deep copy. The in-memory store clones on every read and
// every write, so no caller can ever hold a pointer into store-internal
// memory. Without this, the in-memory store would pass tests that a PostgreSQL
// store — which necessarily hands back copies — could never reproduce.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	c := *j
	c.CancelRequestedAt = cloneTime(j.CancelRequestedAt)
	c.StartedAt = cloneTime(j.StartedAt)
	c.EndedAt = cloneTime(j.EndedAt)
	c.Error = j.Error.Clone()
	c.Steps = make([]*Step, len(j.Steps))
	for i, s := range j.Steps {
		c.Steps[i] = s.Clone()
	}
	return &c
}

// Clone deep-copies a step, including every slice it owns.
func (s *Step) Clone() *Step {
	if s == nil {
		return nil
	}
	c := *s
	c.DependsOn = cloneStrings(s.DependsOn)
	c.Params = cloneRaw(s.Params)
	c.Result = cloneRaw(s.Result)
	c.Error = s.Error.Clone()
	c.ScheduledAt = cloneTime(s.ScheduledAt)
	c.StartedAt = cloneTime(s.StartedAt)
	c.EndedAt = cloneTime(s.EndedAt)
	return &c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// cloneStrings and cloneRaw preserve nil-ness. append(nil, empty...) yields
// nil, which is right, but being explicit keeps round-tripped JSON stable:
// a nil slice marshals to null and an empty one to [], and tests pin both.
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	c := make([]string, len(s))
	copy(c, s)
	return c
}

func cloneRaw(b json.RawMessage) json.RawMessage {
	if b == nil {
		return nil
	}
	c := make(json.RawMessage, len(b))
	copy(c, b)
	return c
}
