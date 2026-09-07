package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// claimable is THE readiness predicate as SQL, and jobstate.Claimable is the
// same expression in Go. They are two spellings of one rule, which is why the
// conformance suite runs identically against both stores.
//
// Readiness is a predicate, never a stored state: no BLOCKED state, no
// materialised pending-dependency counter to drift, and therefore no "who
// unblocks this step" question to get wrong. A step becomes claimable the
// instant its last dependency succeeds, because that is what this query says.
//
// The dependency clause is the doubly-negated form — "there is no dependency
// that is not a SUCCEEDED step" — rather than the shorter
//
//	NOT EXISTS (SELECT 1 FROM job_steps d
//	             WHERE d.job_id = s.job_id AND d.id = ANY (s.depends_on)
//	               AND d.state <> 'SUCCEEDED')
//
// because the short form treats a dependency on a step that does not exist as
// SATISFIED: there is no row, so nothing is found, so nothing blocks. The Go
// predicate treats a missing dependency as unsatisfiable. Plan validation makes
// a dangling dependency impossible today, so the two forms cannot actually
// disagree — but "cannot disagree because of a check somewhere else" is exactly
// the assumption that stops being true later.
//
// $1 is now. Every timestamp is a bind variable rather than now() so a test can
// drive lease expiry and backoff with fabricated time and no sleeping.
const claimablePredicate = `
	    j.cancel_requested_at IS NULL
	AND j.state IN ('QUEUED','RUNNING')
	AND s.state IN ('QUEUED','RETRYING')
	AND s.next_attempt_at <= $1
	AND (s.lease_expires_at IS NULL OR s.lease_expires_at <= $1)
	AND NOT EXISTS (
	      SELECT 1 FROM unnest(s.depends_on) AS dep (id)
	       WHERE NOT EXISTS (
	             SELECT 1 FROM job_steps d
	              WHERE d.job_id = s.job_id AND d.id = dep.id
	                AND d.state = 'SUCCEEDED'))`

// Claim atomically takes up to req.Limit ready steps, moves each to SCHEDULED,
// increments its attempt counter and stamps a fresh fencing token.
//
// The candidate query locks the JOB row — `FOR UPDATE OF j SKIP LOCKED` — not
// the step rows. That is the lock protocol described in the package comment:
// one lock, taken first, on every path, so Claim and Finish cannot deadlock by
// taking two locks in opposite orders. SKIP LOCKED then does what it always
// does: a job another replica is already writing is passed over rather than
// waited on, so N claimers make N claimers' worth of progress.
//
// Two halves of the contract matter to the dispatcher's capacity accounting,
// and internal/storetest asserts both:
//
//   - on error, NO leases are returned. Partial results are never returned.
//   - len(result) <= req.Limit, always. Returning fewer — including zero — is
//     normal and is not an error.
func (s *Store) Claim(ctx context.Context, req runmesh.ClaimRequest) ([]runmesh.Lease, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if req.Limit < 1 {
		return nil, nil
	}

	var leases []runmesh.Lease
	var events []runmesh.Event

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var tools any
		if len(req.Tools) > 0 {
			tools = textArray(req.Tools)
		}

		// ORDER BY job priority, then age, then step id — with the job id as a
		// final tie-break so the ordering is total and reproducible even when
		// two jobs are created in the same instant. Because every job attribute
		// sorts ahead of the step id, a job's steps come out adjacent, which is
		// what lets the loop below process one job at a time without changing
		// the order leases are returned in.
		rows, err := tx.QueryContext(ctx, `
			SELECT s.job_id, s.id
			  FROM job_steps s
			  JOIN jobs j ON j.id = s.job_id
			 WHERE `+claimablePredicate+`
			   AND ($3::text[] IS NULL OR s.tool = ANY ($3::text[]))
			 ORDER BY j.priority DESC, j.created_at, j.id, s.id
			 LIMIT $2
			 FOR UPDATE OF j SKIP LOCKED`, req.Now, req.Limit, tools)
		if err != nil {
			return fmt.Errorf("pgstore: selecting claimable steps: %w", err)
		}

		type candidate struct{ jobID, stepID string }
		var cands []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.jobID, &c.stepID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("pgstore: selecting claimable steps: %w", err)
			}
			cands = append(cands, c)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("pgstore: selecting claimable steps: %w", err)
		}
		_ = rows.Close()
		if len(cands) == 0 {
			return nil
		}

		for i := 0; i < len(cands); {
			jobID := cands[i].jobID
			t, err := lockJob(ctx, tx, jobID)
			if err != nil {
				return err
			}

			for ; i < len(cands) && cands[i].jobID == jobID; i++ {
				step := t.job.Step(cands[i].stepID)
				// The row was claimable when the predicate ran and the job row
				// has been locked ever since, so this cannot be nil and cannot
				// have moved on. Re-checking costs one comparison and turns a
				// future protocol mistake into a skipped step rather than a
				// double-claimed one.
				if step == nil || !jobstate.Claimable(t.job, step, req.Now, nil) {
					continue
				}
				leases = append(leases, claimOne(t, step, req))
			}

			t.reconcile(req.Now)
			if err := t.flush(ctx); err != nil {
				return err
			}
			events = append(events, t.events...)
		}
		return nil
	})
	if err != nil {
		// The contract: on error, no leases at all. The transaction rolled
		// back, so returning the partial slice would hand out fencing tokens
		// for rows that were never written.
		return nil, err
	}

	s.publish(events)
	return leases, nil
}

// claimOne performs the SCHEDULED transition for one step and mints its lease.
func claimOne(t *jobTx, step *runmesh.Step, req runmesh.ClaimRequest) runmesh.Lease {
	scheduledAt := req.Now

	// Attempt is monotonic: it increments here, on every claim, and is never
	// decremented. Failures — the retry budget — is untouched, because being
	// claimed is not a failure. Keeping the two apart is what stops a rolling
	// restart from silently exhausting every in-flight job's retries.
	step.Attempt++
	step.State = runmesh.Scheduled
	step.LeaseID = runmesh.NewID("lse_", req.Now)
	step.LeaseOwner = req.Owner
	step.LeaseExpiresAt = req.Now.Add(req.LeaseTTL)
	step.ScheduledAt = &scheduledAt
	step.EndedAt = nil
	step.StartedAt = nil
	step.Error = nil
	step.Version++

	t.emit(jobstate.StepEvent(runmesh.StepScheduled, step, req.Now))

	return runmesh.Lease{
		ID:             step.LeaseID,
		Owner:          req.Owner,
		JobID:          t.job.ID,
		StepID:         step.ID,
		AttemptID:      runmesh.AttemptID(t.job.ID, step.ID, step.Attempt),
		IdempotencyKey: runmesh.IdempotencyKey(t.job.ID, step.ID),
		Attempt:        step.Attempt,
		Failures:       step.Failures,
		MaxAttempts:    step.MaxAttempts,
		Tool:           step.Tool,
		Params:         append(json.RawMessage(nil), step.Params...),
		Deps:           depResults(t.job, step),
		Timeout:        step.Timeout,
		ClaimedAt:      req.Now,
		ExpiresAt:      step.LeaseExpiresAt,
	}
}

// depResults collects the outputs of a step's dependencies, so a tool consumes
// upstream data without ever being handed a store.
func depResults(j *runmesh.Job, s *runmesh.Step) map[string]json.RawMessage {
	if len(s.DependsOn) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(s.DependsOn))
	for _, id := range s.DependsOn {
		d := j.Step(id)
		if d == nil || len(d.Result) == 0 {
			continue
		}
		out[id] = append(json.RawMessage(nil), d.Result...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// QueueDepth counts the steps that are claimable right now.
//
// It is literally the same predicate Claim uses — counted instead of taken,
// with no locking clause — which is what keeps admission control honest as the
// predicate evolves. A second, hand-maintained "is it ready" expression here
// would drift, and the symptom would be a 429 for a queue that was empty.
func (s *Store) QueueDepth(ctx context.Context, now time.Time) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM job_steps s
		  JOIN jobs j ON j.id = s.job_id
		 WHERE `+claimablePredicate, now).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("pgstore: counting the queue: %w", err)
	}
	return n, nil
}

// ExpireLeases is the crash-recovery sweep: the mechanism that makes a killed
// worker recoverable rather than a job stranded in RUNNING for ever.
//
// This is the method that made Week 1's fault tolerance a claim and makes it a
// fact. In memory, a killed process took the state with it and this sweep
// always found nothing. Against PostgreSQL it finds the leases the dead process
// was holding, and the jobs it was running continue on another replica.
//
// Unlike Release it SPENDS retry budget, because a worker that reliably dies on
// step X must eventually exhaust MaxAttempts rather than crash-loop the whole
// fleet on the same poisoned input.
func (s *Store) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]runmesh.Expired, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}

	var out []runmesh.Expired
	var events []runmesh.Event

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Deliberately NOT filtered on the job's cancel flag: a cancelled job
		// whose worker died still has a lease nobody will renew, and leaving it
		// held would keep the job non-terminal for ever.
		rows, err := tx.QueryContext(ctx, `
			SELECT s.job_id, s.id
			  FROM job_steps s
			  JOIN jobs j ON j.id = s.job_id
			 WHERE s.state IN ('SCHEDULED','RUNNING')
			   AND s.lease_expires_at IS NOT NULL
			   AND s.lease_expires_at <= $1
			 ORDER BY s.job_id, s.id
			 LIMIT $2
			 FOR UPDATE OF j SKIP LOCKED`, now, limit)
		if err != nil {
			return fmt.Errorf("pgstore: selecting expired leases: %w", err)
		}

		type victim struct{ jobID, stepID string }
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.jobID, &v.stepID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("pgstore: selecting expired leases: %w", err)
			}
			victims = append(victims, v)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("pgstore: selecting expired leases: %w", err)
		}
		_ = rows.Close()
		if len(victims) == 0 {
			return nil
		}

		for i := 0; i < len(victims); {
			jobID := victims[i].jobID
			t, err := lockJob(ctx, tx, jobID)
			if err != nil {
				return err
			}

			for ; i < len(victims) && victims[i].jobID == jobID; i++ {
				step := t.job.Step(victims[i].stepID)
				if step == nil || !step.State.Active() {
					continue
				}
				if x, ok := expireOne(t, step, now); ok {
					out = append(out, x)
				}
			}

			t.reconcile(now)
			if err := t.flush(ctx); err != nil {
				return err
			}
			events = append(events, t.events...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.publish(events)
	s.signalReady()
	return out, nil
}

func expireOne(t *jobTx, step *runmesh.Step, now time.Time) (runmesh.Expired, bool) {
	owner := step.LeaseOwner
	step.Failures++

	next := runmesh.Queued
	if step.Failures >= step.MaxAttempts {
		next = runmesh.Failed
	}
	if err := jobstate.Guard(step, next); err != nil {
		// Unreachable for an Active step; the guard stays as a backstop.
		return runmesh.Expired{}, false
	}

	step.State = next
	step.Version++
	jobstate.ClearLease(step)
	switch next {
	case runmesh.Queued:
		// Requeued for immediate retry rather than backed off. The incremented
		// failure count is what bounds a crash loop; adding a second delay
		// mechanism here would mean the retry policy lived in two places.
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
	t.emit(e)

	return runmesh.Expired{
		JobID: t.job.ID, StepID: step.ID, Attempt: step.Attempt,
		Owner: owner, NewState: next,
	}, true
}
