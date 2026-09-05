package engine

import (
	"context"
	"errors"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Stop records WHY a step stopped.
//
// It is assigned BEFORE the step's context is cancelled, and Classify consults
// it before it ever looks at the tool's error. That ordering is the whole
// defence against a cancelled step being misread as a retryable failure: a
// tool that returns Retry("connection reset") on its way out the door cannot
// buy itself a retry, because in that branch its error is never read.
type Stop uint8

const (
	StopNone     Stop = iota // the tool returned on its own
	StopTimeout              // the per-step deadline fired
	StopCancel               // a heartbeat reported Directive.Cancel
	StopLost                 // the lease expired or was stolen
	StopShutdown             // the drain deadline expired
)

func (s Stop) String() string {
	switch s {
	case StopTimeout:
		return "timeout"
	case StopCancel:
		return "cancel"
	case StopLost:
		return "lease_lost"
	case StopShutdown:
		return "shutdown"
	default:
		return "none"
	}
}

// Disposition is the fully decided fate of one attempt.
type Disposition struct {
	State   runmesh.State
	Release bool // return the step to QUEUED, spending no retry budget
	Discard bool // write nothing at all: somebody else owns this step now
	// CountFail spends one unit of retry budget. It is separate from the
	// attempt counter on purpose: an attempt names an execution, a failure
	// spends budget, and a rolling restart must cost the second but not
	// the first.
	CountFail bool
	RetryIn   time.Duration
	Reason    string
	Error     *runmesh.ErrorInfo
}

// Classify is pure, total, and has no clock. Every opinion in this design
// lives here, which is why it is the most heavily table-tested function in the
// codebase: changing a policy means changing one switch and one table, not
// hunting for the branch that decides it.
func Classify(stop Stop, err error, failures, maxAttempts int, b Backoff) Disposition {
	switch stop {
	case StopLost:
		// A new lease-holder owns this step and will write its own outcome.
		// Writing anything here is exactly the zombie-overwrite the fencing
		// token exists to prevent, so we write nothing at all.
		return Disposition{Discard: true, Reason: runmesh.CodeLeaseLost}

	case StopShutdown:
		// Our failure, not the step's. Requeue and spend no budget: a rolling
		// restart must cost zero retries, or a deploy silently consumes every
		// in-flight job's remaining attempts.
		return Disposition{State: runmesh.Queued, Release: true, Reason: runmesh.CodeShutdown}

	case StopCancel:
		// Never counted as a failure and never retried. Somebody asked for
		// this to stop; charging them a retry for complying would be absurd.
		return Disposition{
			State: runmesh.Cancelled,
			Error: &runmesh.ErrorInfo{
				Code:    runmesh.CodeCancelled,
				Message: "cancelled",
				Attempt: failures + 1,
			},
		}

	case StopTimeout:
		info := &runmesh.ErrorInfo{
			Code:      runmesh.CodeTimeout,
			Message:   "step timeout exceeded",
			Retryable: true,
			Attempt:   failures + 1,
		}
		if failures+1 < maxAttempts {
			return Disposition{
				State: runmesh.Retrying, CountFail: true,
				RetryIn: b.Delay(failures), Error: info,
			}
		}
		// The final timeout reports TIMED_OUT rather than FAILED, so the job
		// rollup can say "this timed out" instead of the less useful "this
		// failed" — which is the difference between a user tuning a timeout
		// and a user reading logs.
		return Disposition{State: runmesh.TimedOut, CountFail: true, Error: info}
	}

	// stop == StopNone: the tool returned under its own power.
	if err == nil {
		return Disposition{State: runmesh.Succeeded}
	}

	var te *runmesh.ToolError
	if errors.As(err, &te) {
		info := &runmesh.ErrorInfo{
			Code: te.Code, Message: te.Message,
			Retryable: te.Retryable, Attempt: failures + 1,
		}
		if te.Retryable && failures+1 < maxAttempts {
			d := b.Delay(failures)
			if te.RetryAfter > 0 {
				// A server's own Retry-After is better information than our
				// backoff curve — but it is still capped, so a hostile or
				// buggy upstream cannot park a step for a week.
				d = min(te.RetryAfter, b.Max)
			}
			return Disposition{State: runmesh.Retrying, CountFail: true, RetryIn: d, Error: info}
		}
		return Disposition{State: runmesh.Failed, CountFail: true, Error: info}
	}

	// A tool reporting a context error while nothing cancelled it has broken
	// its contract — most likely by using a context of its own. Retrying it
	// would burn the budget on a bug that will reproduce every time.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Disposition{
			State: runmesh.Failed, CountFail: true,
			Error: &runmesh.ErrorInfo{
				Code:    runmesh.CodeContractBroken,
				Message: err.Error(),
				Attempt: failures + 1,
			},
		}
	}

	// Unclassified errors are TERMINAL, loudly.
	//
	// Under an explicitly at-least-once contract, silently retrying an error
	// nobody classified is how a side-effecting tool ends up running three
	// times. There is deliberately no configuration knob for this: a knob
	// would let the question be deferred for ever. The counter-argument —
	// fail-soft, so a forgotten annotation costs three attempts rather than a
	// whole job — is real, and it is the first thing to revisit if tool
	// authors find this hostile in practice.
	return Disposition{
		State: runmesh.Failed, CountFail: true,
		Error: &runmesh.ErrorInfo{
			Code:    runmesh.CodeUnclassified,
			Message: err.Error(),
			Attempt: failures + 1,
		},
	}
}
