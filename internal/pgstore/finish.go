package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// withLease is the shape every worker write shares: lock the job, resolve the
// lease to a step, run the mutation, reconcile, write back.
//
// Having it in one place is what guarantees the three outcomes a worker
// branches on are produced identically by Start, Finish and Release:
//
//	ErrNotFound   the job or step is gone
//	ErrLeaseLost  the fencing token does not match — write NOTHING
//	ErrConflict   the token matched but the state moved on — discard the result
//
// The distinction is not cosmetic. A worker that treats a lost lease as a
// conflict retries its write; a worker that treats a conflict as a lost lease
// gives up on a step it still owns. Both are corruption, and both are the kind
// of thing that only happens under the contention this store is now exposed to.
func (s *Store) withLease(
	ctx context.Context,
	l runmesh.Lease,
	now time.Time,
	mutate func(t *jobTx, step *runmesh.Step) error,
) error {
	if err := s.guard(ctx); err != nil {
		return err
	}

	var events []runmesh.Event
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := lockJob(ctx, tx, l.JobID)
		if err != nil {
			return err
		}
		step, err := jobstate.Lookup(t.job, l)
		if err != nil {
			return err
		}
		if err := mutate(t, step); err != nil {
			return err
		}
		t.reconcile(now)
		if err := t.flush(ctx); err != nil {
			return err
		}
		events = t.events
		return nil
	})
	if err != nil {
		return err
	}
	s.publish(events)
	return nil
}

// Start moves a claimed step from SCHEDULED to RUNNING.
//
// It is a separate call from Claim because the gap between "claimed" and
// "actually executing" is microseconds today and seconds in Week 3, when it is
// a pod waiting to be scheduled. The execution waterfall needs that gap as its
// own span, so the runtime has to record it as its own transition.
func (s *Store) Start(ctx context.Context, l runmesh.Lease, now time.Time) error {
	return s.withLease(ctx, l, now, func(t *jobTx, step *runmesh.Step) error {
		if err := jobstate.Guard(step, runmesh.Running); err != nil {
			return err
		}
		at := now
		step.State = runmesh.Running
		step.StartedAt = &at
		step.Version++
		t.emit(jobstate.StepEvent(runmesh.StepStarted, step, now))
		return nil
	})
}

// Finish applies a step's terminal (or RETRYING) transition, appends its events
// and recomputes the job — all in ONE transaction, which is the whole reason
// this design needed no dual write and no outbox.
//
// It is guarded on the fencing token AND on the current state. Under PostgreSQL
// both guards are load-bearing for the first time: two replicas really can
// reach this row at once, and the zombie writer this rejects is a process that
// hung, lost its lease to the sweep, and woke up with a result for an attempt
// somebody else has already redone.
func (s *Store) Finish(ctx context.Context, o runmesh.Outcome) error {
	ended := o.EndedAt
	if ended.IsZero() {
		ended = o.StartedAt
	}
	return s.finish(ctx, o, ended)
}

func (s *Store) finish(ctx context.Context, o runmesh.Outcome, ended time.Time) error {
	err := s.withLease(ctx, o.Lease, ended, func(t *jobTx, step *runmesh.Step) error {
		if !step.State.Active() {
			return fmt.Errorf("%w: step is %s, not active", runmesh.ErrConflict, step.State)
		}

		to := o.State
		// A retry on a job somebody has asked to cancel would restart work the
		// operator has already stopped. The attempt that just happened is still
		// recorded honestly — it really did run — but it does not get another.
		if to == runmesh.Retrying && t.job.CancelRequestedAt != nil {
			to = runmesh.Cancelled
		}
		if err := jobstate.Guard(step, to); err != nil {
			return err
		}

		step.State = to
		step.Error = o.Error.Clone()
		step.EndedAt = &ended
		if o.CountFail {
			step.Failures++
		}
		if len(o.Result) > 0 {
			step.Result = append(json.RawMessage(nil), o.Result...)
		}
		jobstate.ClearLease(step)
		step.Version++

		evt := runmesh.StepFinished
		if to == runmesh.Retrying {
			// The backoff IS next_attempt_at. There is no timer goroutine per
			// retrying step: the step is simply not claimable until the clock
			// passes this instant, which costs nothing while a million steps
			// wait — and, now that it is a column, survives a restart.
			step.NextAttemptAt = o.NextAttemptAt
			step.Result = nil
			evt = runmesh.StepRetryScheduled
		}

		e := jobstate.StepEvent(evt, step, ended)
		if !o.StartedAt.IsZero() {
			e.DurationMS = ended.Sub(o.StartedAt).Milliseconds()
		}
		if to == runmesh.Retrying {
			e.Attrs = map[string]any{
				"next_attempt_at": step.NextAttemptAt,
				"failures":        step.Failures,
			}
		}
		t.emit(e)
		return nil
	})
	if err != nil {
		return err
	}
	s.signalReady()
	return nil
}

// Release returns a step to QUEUED WITHOUT spending retry budget.
//
// The asymmetry with ExpireLeases is the subtlest rule in the design, and it is
// deliberate: a graceful release is OUR failure — a deploy, a drain — so a
// rolling restart must cost zero retries. A lease that simply expired is
// different, because a worker that reliably dies on one step must eventually
// exhaust MaxAttempts rather than crash-loop the fleet.
//
// Attempt is not decremented by either. It names the execution, so it only ever
// goes up.
func (s *Store) Release(ctx context.Context, l runmesh.Lease, reason string, now time.Time) error {
	err := s.withLease(ctx, l, now, func(t *jobTx, step *runmesh.Step) error {
		if !step.State.Active() {
			return fmt.Errorf("%w: step is %s, not active", runmesh.ErrConflict, step.State)
		}
		if err := jobstate.Guard(step, runmesh.Queued); err != nil {
			return err
		}

		step.State = runmesh.Queued
		step.NextAttemptAt = now
		step.StartedAt = nil
		step.ScheduledAt = nil
		jobstate.ClearLease(step)
		step.Version++

		e := jobstate.StepEvent(runmesh.StepReleased, step, now)
		e.Attrs = map[string]any{"reason": reason}
		t.emit(e)

		// The reconcile that follows (in withLease) is not optional here. If the
		// job is already cancel-flagged, it turns this step straight into
		// CANCELLED; without it the step would sit QUEUED for ever, because the
		// claim predicate excludes cancel-flagged jobs, and the job would never
		// become quiescent.
		return nil
	})
	if err != nil {
		return err
	}
	s.signalReady()
	return nil
}

// Heartbeat extends a lease AND is the only channel by which a running step
// learns it should stop.
//
// Renewal and cancellation travel on one round trip because that is the only
// mechanism that works when the cancelling API call lands on a different
// replica from the one executing the step — which, from this week, it does.
//
// It is the ONE mutation that does not take the job lock. It is the hottest
// write in the system, it only ever extends a lease on a step a worker already
// owns, and that is a state Claim never selects and the rollup never reads.
// Keeping it out of the lock graph costs a `jobs.updated_at` that does not
// advance on every beat and buys a heartbeat that can never queue behind
// another job's terminal write.
func (s *Store) Heartbeat(ctx context.Context, l runmesh.Lease, now, until time.Time) (runmesh.Directive, error) {
	if err := s.guard(ctx); err != nil {
		return runmesh.Directive{}, err
	}

	var dir runmesh.Directive
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var (
			state    string
			leaseID  sql.NullString
			cancelAt sql.NullTime
			reason   string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT s.state, s.lease_id, j.cancel_requested_at, j.cancel_reason
			  FROM job_steps s
			  JOIN jobs j ON j.id = s.job_id
			 WHERE s.job_id = $1 AND s.id = $2`,
			l.JobID, l.StepID).Scan(&state, &leaseID, &cancelAt, &reason)
		if errors.Is(err, sql.ErrNoRows) {
			return runmesh.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("pgstore: reading step %s/%s: %w", l.JobID, l.StepID, err)
		}

		if !leaseID.Valid || leaseID.String == "" || leaseID.String != l.ID {
			return fmt.Errorf("%w: step %s/%s", runmesh.ErrLeaseLost, l.JobID, l.StepID)
		}
		st, err := runmesh.ParseState(state)
		if err != nil {
			return err
		}
		if !st.Active() {
			return fmt.Errorf("%w: step is %s, not active", runmesh.ErrConflict, st)
		}

		// Guarded on the token again, so a lease reclaimed between the read and
		// the write cannot be extended by its previous holder.
		res, err := tx.ExecContext(ctx, `
			UPDATE job_steps
			   SET lease_expires_at = $3, version = version + 1
			 WHERE job_id = $1 AND id = $2 AND lease_id = $4`,
			l.JobID, l.StepID, until, l.ID)
		if err != nil {
			return fmt.Errorf("pgstore: extending lease %s/%s: %w", l.JobID, l.StepID, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("%w: step %s/%s", runmesh.ErrLeaseLost, l.JobID, l.StepID)
		}

		dir = runmesh.Directive{
			Cancel: cancelAt.Valid,
			Reason: runmesh.CancelReason(reason),
			Until:  until,
		}
		return nil
	})
	if err != nil {
		return runmesh.Directive{}, err
	}
	return dir, nil
}
