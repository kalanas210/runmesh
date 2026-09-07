package runmesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// ClaimRequest is, verbatim, the parameter list of the SKIP LOCKED query in
// internal/pgstore. Now is a PARAMETER rather than a clock read in the store: that is
// how you write SQL ($1), and it is what makes every lease-expiry and backoff
// case in the shared conformance suite a pure function call with no goroutine
// and no fake clock.
type ClaimRequest struct {
	Owner    string // stable per process: hostname-pid-random
	Limit    int    // never larger than the dispatcher's free capacity
	LeaseTTL time.Duration
	Now      time.Time
	// Tools is a capability filter; nil means any. Week 3 uses it to split
	// in-process tools from container tools across worker fleets.
	Tools []string
}

// Lease is the capability a worker holds over one step for one attempt. It is
// the ONLY handle a worker gets: a worker never holds a *Step, so it cannot
// tell whether the claim came from an in-process dispatcher or from
// SELECT ... FOR UPDATE SKIP LOCKED running on another node.
type Lease struct {
	ID    string // fresh fencing token per claim
	Owner string

	JobID  string
	StepID string

	// AttemptID names THIS execution (job.step.N) and becomes the Week-3
	// Kubernetes Job name. IdempotencyKey is stable across every attempt of
	// this step; it is the value a side-effecting tool uses so a retry does
	// not duplicate the effect.
	AttemptID      string
	IdempotencyKey string

	Attempt     int
	Failures    int
	MaxAttempts int

	Tool   string
	Params json.RawMessage
	// Deps carries the results of this step's depends_on steps, so a tool can
	// consume upstream output without being handed a store.
	Deps    map[string]json.RawMessage
	Timeout time.Duration

	ClaimedAt time.Time
	ExpiresAt time.Time
}

// Outcome is a fully decided result. The worker classifies (engine.Classify,
// pure) and computes the backoff (engine.Backoff.Delay, pure); the store's job
// is to make the decision atomic and legal, not to make the decision.
type Outcome struct {
	Lease  Lease
	State  State // Succeeded | Failed | TimedOut | Cancelled | Retrying
	Result json.RawMessage
	Error  *ErrorInfo
	// NextAttemptAt is set only when State == Retrying.
	NextAttemptAt time.Time
	// CountFail reports whether this outcome spends a unit of retry budget.
	CountFail bool
	StartedAt time.Time
	EndedAt   time.Time
}

// Directive is what a heartbeat tells a running step. Lease renewal and
// cancellation travel on ONE round trip, because that is the only mechanism
// that still works when the cancelling API call lands on a different process
// from the one executing the step.
type Directive struct {
	Cancel bool
	Reason CancelReason
	Until  time.Time
}

// Expired is one reclaimed lease, returned by the reconciler's sweep.
type Expired struct {
	JobID    string
	StepID   string
	Attempt  int
	Owner    string
	NewState State // Queued (will retry) or Failed (budget exhausted)
}

// JobFilter is the query behind GET /api/v1/jobs. Cursor is the last id of the
// previous page: ids are k-sortable, so keyset pagination needs no second
// column and never uses OFFSET.
type JobFilter struct {
	States []State
	Cursor string
	Limit  int
}

// JobPage is one page of jobs plus the cursor that follows it.
type JobPage struct {
	Jobs       []*Job
	NextCursor string
}

// AttemptID names ONE execution of one step. It is deterministic given
// (job, step, attempt), so the Week-3 executor's create call is idempotent if
// its response is lost within a single attempt. A retry gets a new AttemptID
// and therefore a new workload; safety across attempts comes from
// IdempotencyKey, not from adopting the previous attempt's pod.
func AttemptID(jobID, stepID string, attempt int) string {
	return jobID + "." + stepID + "." + strconv.Itoa(attempt)
}

// IdempotencyKey is STABLE across every attempt of a step. It is the third
// column of UNIQUE(job_id, step_id, idempotency_key): the value a
// side-effecting tool uses so that a retry does not duplicate the effect.
//
// It is a hash rather than the raw pair so it is fixed-width and safe to hand
// to an external system as an opaque token.
func IdempotencyKey(jobID, stepID string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + stepID))
	return hex.EncodeToString(sum[:16])
}
