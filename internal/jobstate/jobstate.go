// Package jobstate is the transition policy every store shares: what makes a
// step claimable, what a job's state rolls up to, what a failure dooms, and
// what a cancellation touches.
//
// It exists because Week 2 adds a SECOND store. The obvious way to write one is
// to reimplement the policy in SQL and trust the conformance suite to catch any
// divergence. The suite is good, but it can only catch the cases somebody
// thought to write down — and this policy is the subtlest code in the project:
// the fail-fast trigger that has to fire from every writer, the cancel
// propagation that must not touch a leased step, the doomed-dependent sweep
// that stops ContinueOnFailure hanging for ever.
//
// So the two stores do not agree by testing. They agree by calling the same
// functions. What internal/storetest then proves is the part that genuinely
// differs between them: the claim predicate as real SKIP LOCKED SQL, fencing
// under real contention, and paging over real rows.
//
// Everything here is PURE: no clock, no lock, no IO, no store. `now` is always
// a parameter, exactly as it is a bind variable in SQL, and events are handed
// to an Emit callback because numbering them is the one part that is genuinely
// each store's own business.
package jobstate

import (
	"fmt"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Emit records an event produced by a transition. Each store supplies its own:
// memstore stamps the sequence numbers inside its mutex, pgstore inside the
// transaction that wrote the transition. Both must assign Seq in the same
// critical section as the state change, or two concurrent appends can read the
// same MAX(seq) and leave a gap in a cursor clients resume from.
type Emit func(runmesh.Event)

// Claimable is THE readiness predicate, and it is the same expression as the
// SKIP LOCKED query in internal/pgstore, written in Go:
//
//	    j.cancel_requested_at IS NULL
//	AND j.state IN ('QUEUED','RUNNING')
//	AND s.state IN ('QUEUED','RETRYING')
//	AND s.next_attempt_at <= $1
//	AND (s.lease_expires_at IS NULL OR s.lease_expires_at <= $1)
//	AND NOT EXISTS (
//	      SELECT 1 FROM unnest(s.depends_on) AS dep (id)
//	       WHERE NOT EXISTS (
//	             SELECT 1 FROM job_steps d
//	              WHERE d.job_id = s.job_id AND d.id = dep.id
//	                AND d.state = 'SUCCEEDED'))
//
// The dependency clause is doubly negated — "there is no dependency that is not
// a SUCCEEDED step" — so that a dependency naming a step which does not exist
// blocks rather than vanishes, which is what the loop below does. See the
// comment on claimablePredicate in internal/pgstore for why the shorter form
// would quietly disagree.
//
// Readiness is a predicate, never a stored state. There is no BLOCKED state, no
// materialised pending-dependency counter that can drift, and therefore no "who
// unblocks this step" question to get wrong: a step becomes claimable the
// instant its last dependency succeeds, because that is what the query says.
//
// tools is a capability filter; nil means any.
func Claimable(j *runmesh.Job, s *runmesh.Step, now time.Time, tools map[string]bool) bool {
	if j.CancelRequestedAt != nil {
		return false
	}
	if j.State != runmesh.Queued && j.State != runmesh.Running {
		return false
	}
	if s.State != runmesh.Queued && s.State != runmesh.Retrying {
		return false
	}
	if s.NextAttemptAt.After(now) {
		return false
	}
	if !s.LeaseExpiresAt.IsZero() && s.LeaseExpiresAt.After(now) {
		return false
	}
	if len(tools) > 0 && !tools[s.Tool] {
		return false
	}
	for _, dep := range s.DependsOn {
		d := j.Step(dep)
		if d == nil || d.State != runmesh.Succeeded {
			return false
		}
	}
	return true
}

// Guard rejects a transition the state machine does not allow. It is the third
// of three redundant gates: a store's guarded write is the real enforcement,
// runmesh.CanStep is data a table test walks, and this is where the two meet on
// every single mutation.
func Guard(s *runmesh.Step, to runmesh.State) error {
	if !runmesh.CanStep(s.State, to) {
		return fmt.Errorf("%w: step is %s, cannot move to %s", runmesh.ErrConflict, s.State, to)
	}
	return nil
}

// ClearLease drops a step's fencing token, so a worker still holding it gets
// ErrLeaseLost on its next write rather than clobbering whoever comes next.
func ClearLease(s *runmesh.Step) {
	s.LeaseID = ""
	s.LeaseOwner = ""
	s.LeaseExpiresAt = time.Time{}
}

// StepEvent builds an event about one step from its current row.
func StepEvent(t runmesh.EventType, s *runmesh.Step, at time.Time) runmesh.Event {
	return runmesh.Event{
		StepID:  s.ID,
		Attempt: s.Attempt,
		Type:    t,
		At:      at,
		State:   s.State,
		Error:   s.Error.Clone(),
	}
}

// Reconcile is the one place a job's own state is decided. Every mutating store
// method ends here, so there is exactly one implementation of "what does this
// job look like now" rather than one per call site — and, since Week 2, one
// across both stores rather than one per store.
//
// It mutates j in place and hands every event it produces to emit.
func Reconcile(j *runmesh.Job, now time.Time, emit Emit) {
	// Fail-fast and operator cancel are ONE mechanism with two triggers: a
	// terminal step failure under FailFast raises the same cancel flag an
	// operator would, and sibling steps then stop through exactly the same path.
	//
	// This lives HERE, and not in the Finish path, because Finish is not the
	// only writer that can produce a terminal step failure — the lease sweep
	// does too, when a dead worker's step exhausts its retry budget. Putting
	// the trigger in one call site left that path without it: the step went
	// FAILED, no cancel flag was raised, MarkDoomed never ran because the policy
	// was not ContinueOnFailure, and the dependents sat QUEUED for ever while
	// the job stayed RUNNING. Making it a property of the transition rather than
	// of the caller means every present and future writer — in either store —
	// inherits it.
	if j.OnStepFailure == runmesh.FailFast && j.CancelRequestedAt == nil {
		for _, step := range j.Steps {
			if step.State != runmesh.Failed && step.State != runmesh.TimedOut {
				continue
			}
			at := now
			j.CancelRequestedAt = &at
			j.CancelReason = runmesh.CancelStepFailed
			emit(runmesh.Event{
				Type:  runmesh.JobCancelRequested,
				At:    now,
				State: j.State,
				Attrs: map[string]any{
					"reason":  string(runmesh.CancelStepFailed),
					"step_id": step.ID,
				},
			})
			break
		}
	}

	// A cancel-flagged job cancels everything that has not started. Steps a
	// worker owns are left alone: they learn through their next heartbeat, and
	// reaching in here would be the zombie-overwrite the fencing token exists to
	// prevent.
	if j.CancelRequestedAt != nil {
		for _, step := range j.Steps {
			if step.State != runmesh.Queued && step.State != runmesh.Retrying {
				continue
			}
			ended := now
			step.State = runmesh.Cancelled
			step.EndedAt = &ended
			step.Error = &runmesh.ErrorInfo{
				Code:    runmesh.CodeCancelled,
				Message: "cancelled before this step started",
				Attempt: step.Failures,
			}
			ClearLease(step)
			step.Version++
			emit(StepEvent(runmesh.StepFinished, step, now))
		}
	} else if j.OnStepFailure == runmesh.ContinueOnFailure {
		for _, step := range MarkDoomed(j, now) {
			emit(StepEvent(runmesh.StepFinished, step, now))
		}
	}

	next, info := Rollup(j)
	j.UpdatedAt = now
	j.Version++

	if next == j.State {
		return
	}
	if !runmesh.CanJob(j.State, next) {
		// Unreachable by construction; if it ever happens, the state machine and
		// the rollup disagree, and silently applying the transition would hide
		// which one is wrong.
		panic(fmt.Sprintf("jobstate: illegal job transition %s -> %s for %s", j.State, next, j.ID))
	}

	j.State = next
	j.Error = info
	switch {
	case next == runmesh.Running && j.StartedAt == nil:
		at := now
		j.StartedAt = &at
		emit(runmesh.Event{Type: runmesh.JobStarted, At: now, State: next})
	case next.Terminal():
		at := now
		j.EndedAt = &at
		e := runmesh.Event{Type: runmesh.JobFinished, At: now, State: next, Error: info.Clone()}
		if j.StartedAt != nil {
			e.DurationMS = now.Sub(*j.StartedAt).Milliseconds()
		}
		emit(e)
	}
}

// Rollup derives a job's state from its steps.
//
// Precedence, once every step is terminal:
//
//  1. an operator cancel wins outright
//  2. TIMED_OUT
//  3. FAILED
//  4. CANCELLED
//  5. SUCCEEDED
//
// Rule 1 exists so that a job an operator cancelled reports CANCELLED even if
// one in-flight step happened to fail on its way out the door — the operator's
// intent is the more useful answer. Rules 2 to 4 mean a fail-fast job reports
// the failing step's own state, which is why TIMED_OUT is meaningful at job
// level without a job-level deadline existing.
func Rollup(j *runmesh.Job) (runmesh.State, *runmesh.ErrorInfo) {
	quiescent, active, started := true, false, j.StartedAt != nil
	var timedOut, failed, cancelled *runmesh.Step

	for _, s := range j.Steps {
		if !s.State.Terminal() {
			quiescent = false
		}
		if s.State.Active() {
			active = true
		}
		if s.ScheduledAt != nil || s.StartedAt != nil {
			started = true
		}
		switch s.State {
		case runmesh.TimedOut:
			if timedOut == nil {
				timedOut = s
			}
		case runmesh.Failed:
			if failed == nil {
				failed = s
			}
		case runmesh.Cancelled:
			if cancelled == nil {
				cancelled = s
			}
		}
	}

	if !quiescent {
		// A job with work outstanding is RUNNING from the moment a step is
		// claimed, not from the moment a tool is invoked. In Week 3 the gap
		// between those is a pod waiting to be scheduled, and a job whose pod is
		// pending is not "queued" in any sense a user would recognise.
		if active || started {
			return runmesh.Running, nil
		}
		return runmesh.Queued, nil
	}

	if j.CancelRequestedAt != nil && j.CancelReason == runmesh.CancelUser {
		return runmesh.Cancelled, &runmesh.ErrorInfo{
			Code:    runmesh.CodeCancelled,
			Message: "cancelled by request",
		}
	}
	switch {
	case timedOut != nil:
		return runmesh.TimedOut, timedOut.Error.Clone()
	case failed != nil:
		return runmesh.Failed, failed.Error.Clone()
	case cancelled != nil:
		return runmesh.Cancelled, cancelled.Error.Clone()
	default:
		return runmesh.Succeeded, nil
	}
}

// MarkDoomed cancels every step that can never become claimable because
// something it transitively depends on ended in a non-successful terminal
// state. It is a BFS over the forward edges of the DAG.
//
// Without it, ContinueOnFailure would hang: nine good CSV analyses finish, the
// tenth fails, and the report step that depends on all ten sits QUEUED for ever
// because its dependency will never succeed. That use case needs the other nine
// results, and it needs the job to finalise.
//
// Returns the steps it changed so the caller can emit their events.
func MarkDoomed(j *runmesh.Job, now time.Time) []*runmesh.Step {
	// dependents maps a step id to the steps that depend on it.
	dependents := make(map[string][]*runmesh.Step, len(j.Steps))
	for _, s := range j.Steps {
		for _, dep := range s.DependsOn {
			dependents[dep] = append(dependents[dep], s)
		}
	}

	queue := make([]string, 0, len(j.Steps))
	for _, s := range j.Steps {
		if s.State.Terminal() && s.State != runmesh.Succeeded {
			queue = append(queue, s.ID)
		}
	}

	var changed []*runmesh.Step
	seen := make(map[string]bool, len(j.Steps))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true

		for _, dep := range dependents[id] {
			// An Active step is left alone: a worker owns it and will write its
			// own outcome. Reaching in here would be the zombie-overwrite the
			// fencing token exists to prevent.
			if dep.State != runmesh.Queued && dep.State != runmesh.Retrying {
				continue
			}
			dep.State = runmesh.Cancelled
			dep.Error = &runmesh.ErrorInfo{
				Code:    runmesh.CodeDepFailed,
				Message: "a dependency did not succeed",
				Attempt: dep.Failures,
			}
			ended := now
			dep.EndedAt = &ended
			dep.Version++
			ClearLease(dep)
			changed = append(changed, dep)
			queue = append(queue, dep.ID)
		}
	}
	return changed
}

// Lookup resolves the step a lease refers to, discriminating the two failure
// modes callers branch on. Getting that discrimination right is what stops a
// worker whose lease was reclaimed from clobbering the new holder's result:
// ErrLeaseLost means "write nothing at all", while ErrConflict — raised by the
// caller's own state check — means "the state moved on".
//
// A step with no lease at all is ALSO ErrLeaseLost rather than ErrConflict.
// That is the point of the fencing design: a QUEUED step cannot be written to
// with a valid token, because it has no token to match.
func Lookup(j *runmesh.Job, l runmesh.Lease) (*runmesh.Step, error) {
	step := j.Step(l.StepID)
	if step == nil {
		return nil, runmesh.ErrNotFound
	}
	if step.LeaseID == "" || step.LeaseID != l.ID {
		return nil, fmt.Errorf("%w: step %s/%s", runmesh.ErrLeaseLost, l.JobID, l.StepID)
	}
	return step, nil
}
