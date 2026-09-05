package memstore

import (
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// rollup derives a job's state from its steps. It is PURE — no clock, no lock,
// no IO — so the precedence rules can be table-tested exhaustively without
// constructing a store, and so the same function can run inside a PostgreSQL
// transaction in Week 2.
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
func rollup(j *runmesh.Job) (runmesh.State, *runmesh.ErrorInfo) {
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
		// between those is a pod waiting to be scheduled, and a job whose pod
		// is pending is not "queued" in any sense a user would recognise.
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

// markDoomed cancels every step that can never become claimable because
// something it transitively depends on ended in a non-successful terminal
// state. It is a BFS over the forward edges of the DAG, and it is PURE apart
// from the mutations it is asked to make.
//
// Without it, ContinueOnFailure would hang: nine good CSV analyses finish, the
// tenth fails, and the report step that depends on all ten sits QUEUED for
// ever because its dependency will never succeed. Plan section 4's first use
// case needs the other nine results, and it needs the job to finalise.
//
// Returns the steps it changed so the caller can emit their events.
func markDoomed(j *runmesh.Job, now time.Time) []*runmesh.Step {
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
			clearLease(dep)
			changed = append(changed, dep)
			queue = append(queue, dep.ID)
		}
	}
	return changed
}

func clearLease(s *runmesh.Step) {
	s.LeaseID = ""
	s.LeaseOwner = ""
	s.LeaseExpiresAt = time.Time{}
}
