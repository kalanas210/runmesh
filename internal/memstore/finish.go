package memstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Start moves a claimed step from SCHEDULED to RUNNING.
//
// It is a separate call from Claim because the gap between "claimed" and
// "actually executing" is microseconds in Week 1 and seconds in Week 3, when
// it is a pod waiting to be scheduled. The execution waterfall needs that gap
// as its own span, so the runtime has to record it as its own transition.
func (s *Store) Start(ctx context.Context, l runmesh.Lease, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.ErrClosed
	}

	j, step, err := s.lookupLocked(l)
	if err != nil {
		return err
	}
	if err := jobstate.Guard(step, runmesh.Running); err != nil {
		return err
	}

	at := now
	step.State = runmesh.Running
	step.StartedAt = &at
	step.Version++

	s.appendEventLocked(j, jobstate.StepEvent(runmesh.StepStarted, step, now))
	s.reconcileJobLocked(j, now)
	return nil
}

// Heartbeat extends a lease AND is the only channel by which a running step
// learns it should stop.
//
// Renewal and cancellation travel on one round trip because that is the only
// mechanism that still works when the cancelling API call lands on a different
// replica from the one executing the step. An in-process
// map[jobID]context.CancelFunc would work beautifully today and be deleted in
// a fortnight.
func (s *Store) Heartbeat(ctx context.Context, l runmesh.Lease, now, until time.Time) (runmesh.Directive, error) {
	if err := ctx.Err(); err != nil {
		return runmesh.Directive{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.Directive{}, runmesh.ErrClosed
	}

	j, step, err := s.lookupLocked(l)
	if err != nil {
		return runmesh.Directive{}, err
	}
	if !step.State.Active() {
		return runmesh.Directive{}, fmt.Errorf("%w: step is %s, not active", runmesh.ErrConflict, step.State)
	}

	step.LeaseExpiresAt = until
	step.Version++

	// The JOB row is deliberately not touched, only the step's. In pgstore a
	// heartbeat takes no job lock at all — it is the hottest write in the
	// system, and putting it in the same lock graph as every terminal write to
	// advance a cosmetic updated_at would be a bad trade. This store follows
	// suit rather than being subtly more helpful: two stores that differ in
	// what a heartbeat updates is exactly the divergence this design exists to
	// prevent, even when the field is only informational.

	return runmesh.Directive{
		Cancel: j.CancelRequestedAt != nil,
		Reason: j.CancelReason,
		Until:  until,
	}, nil
}

// Finish applies a step's terminal (or RETRYING) transition, appends its
// events and recomputes the job — all in one critical section, which is the
// same thing pgstore does as one UPDATE plus one INSERT in one transaction.
//
// It is guarded on the fencing token AND on the current state. A worker that
// lost its lease gets ErrLeaseLost and must write nothing; a worker whose step
// moved on gets ErrConflict and must discard its result. Those two branches
// are what stop a reclaimed step from being clobbered by the zombie that lost
// it — and because the worker already handles both, PostgreSQL's behaviour
// under real contention is not a new code path.
func (s *Store) Finish(ctx context.Context, o runmesh.Outcome) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.ErrClosed
	}

	j, step, err := s.lookupLocked(o.Lease)
	if err != nil {
		return err
	}
	if !step.State.Active() {
		return fmt.Errorf("%w: step is %s, not active", runmesh.ErrConflict, step.State)
	}

	to := o.State
	// A retry on a job somebody has asked to cancel would restart work the
	// operator has already stopped. The attempt that just happened is still
	// recorded honestly — it really did run — but it does not get another one.
	if to == runmesh.Retrying && j.CancelRequestedAt != nil {
		to = runmesh.Cancelled
	}
	if err := jobstate.Guard(step, to); err != nil {
		return err
	}

	ended := o.EndedAt
	if ended.IsZero() {
		ended = o.StartedAt
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
		// passes this instant, which is one column in pgstore and costs
		// nothing while a million steps wait.
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
	s.appendEventLocked(j, e)

	s.reconcileJobLocked(j, ended)
	defer s.signalReady()
	return nil
}

// Release returns a step to QUEUED WITHOUT spending retry budget.
//
// The asymmetry with ExpireLeases is the subtlest rule in the design, and it
// is deliberate: a graceful release is OUR failure, not the step's, so a
// rolling restart must cost zero retries. A lease that simply expired is
// different — see ExpireLeases.
//
// Attempt is not decremented by either. It names the execution, so it only
// ever goes up.
func (s *Store) Release(ctx context.Context, l runmesh.Lease, reason string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.ErrClosed
	}

	j, step, err := s.lookupLocked(l)
	if err != nil {
		return err
	}
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
	s.appendEventLocked(j, e)

	// If the job is already cancel-flagged, reconcile immediately turns this
	// step into CANCELLED. Without that, a step released during the drain of a
	// cancelled job would sit QUEUED for ever: the claim predicate excludes
	// cancel-flagged jobs, so nothing would ever pick it up, and the job would
	// never become quiescent.
	s.reconcileJobLocked(j, now)
	defer s.signalReady()
	return nil
}

// ExpireLeases is the crash-recovery sweep: the mechanism that makes a killed
// worker recoverable rather than a job stranded in RUNNING for ever.
//
// Unlike Release it SPENDS retry budget, because a worker that reliably dies
// on step X must eventually exhaust MaxAttempts rather than crash-loop the
// whole fleet on the same poisoned input.
func (s *Store) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]runmesh.Expired, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}

	type victim struct {
		job  *runmesh.Job
		step *runmesh.Step
	}
	var victims []victim
	for _, j := range s.jobs {
		for _, step := range j.Steps {
			if step.State.Active() && !step.LeaseExpiresAt.IsZero() && !step.LeaseExpiresAt.After(now) {
				victims = append(victims, victim{j, step})
			}
		}
	}
	if len(victims) == 0 {
		return nil, nil
	}
	// Deterministic order so the sweep is reproducible under test.
	sort.Slice(victims, func(a, b int) bool {
		if victims[a].job.ID != victims[b].job.ID {
			return victims[a].job.ID < victims[b].job.ID
		}
		return victims[a].step.ID < victims[b].step.ID
	})
	if len(victims) > limit {
		victims = victims[:limit]
	}

	out := make([]runmesh.Expired, 0, len(victims))
	touched := make(map[*runmesh.Job]bool, len(victims))
	for _, v := range victims {
		step, j := v.step, v.job
		owner := step.LeaseOwner
		step.Failures++

		next := runmesh.Queued
		if step.Failures >= step.MaxAttempts {
			next = runmesh.Failed
		}
		if err := jobstate.Guard(step, next); err != nil {
			continue // unreachable for Active steps; the guard stays as a backstop
		}

		step.State = next
		step.Version++
		jobstate.ClearLease(step)
		switch next {
		case runmesh.Queued:
			// Requeued for immediate retry rather than backed off. The
			// incremented failure count is what bounds a crash loop; adding a
			// second delay mechanism here would mean the retry policy lived in
			// two places.
			step.NextAttemptAt = now
			step.StartedAt = nil
			step.ScheduledAt = nil
		case runmesh.Failed:
			ended := now
			step.EndedAt = &ended
			step.Error = &runmesh.ErrorInfo{
				Code:      runmesh.CodeLeaseLost,
				Message:   "worker lease expired and the retry budget is exhausted",
				Retryable: true,
				Attempt:   step.Failures,
			}
		}

		e := jobstate.StepEvent(runmesh.StepLeaseExpired, step, now)
		e.Attrs = map[string]any{"owner": owner, "failures": step.Failures}
		s.appendEventLocked(j, e)

		out = append(out, runmesh.Expired{
			JobID: j.ID, StepID: step.ID, Attempt: step.Attempt,
			Owner: owner, NewState: next,
		})
		touched[j] = true
	}

	for j := range touched {
		s.reconcileJobLocked(j, now)
	}
	defer s.signalReady()
	return out, nil
}

// reconcileJobLocked applies the shared transition policy and records the
// events it produces.
//
// The policy itself lives in internal/jobstate, not here, because there are two
// stores: the fail-fast trigger, the cancel propagation and the
// doomed-dependent sweep are the subtlest rules in the design, and two stores
// that merely pass the same tests are two places for them to drift. Both call
// this one implementation instead.
//
// The caller must hold s.mu.
func (s *Store) reconcileJobLocked(j *runmesh.Job, now time.Time) {
	jobstate.Reconcile(j, now, func(e runmesh.Event) { s.appendEventLocked(j, e) })
}
