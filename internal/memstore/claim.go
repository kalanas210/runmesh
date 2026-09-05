package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// claimable is THE readiness predicate, and it is the same expression as the
// Week-2 SKIP LOCKED query written in Go:
//
//	job.cancel_requested_at IS NULL
//	AND job.state IN ('QUEUED','RUNNING')
//	AND step.state IN ('QUEUED','RETRYING')
//	AND step.next_attempt_at <= $now
//	AND (step.lease_expires_at IS NULL OR step.lease_expires_at <= $now)
//	AND NOT EXISTS (SELECT 1 FROM job_steps d
//	                 WHERE d.job_id = s.job_id AND d.id = ANY(s.depends_on)
//	                   AND d.state <> 'SUCCEEDED')
//
// Readiness is a predicate, never a stored state. There is no BLOCKED state,
// no materialised pending-dependency counter that can drift, and therefore no
// "who unblocks this step" question to get wrong: a step becomes claimable the
// instant its last dependency succeeds, because that is what the query says.
func claimable(j *runmesh.Job, s *runmesh.Step, now time.Time, tools map[string]bool) bool {
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

// candidate pairs a claimable step with its job for ordering.
type candidate struct {
	job  *runmesh.Job
	step *runmesh.Step
}

// Claim atomically takes up to req.Limit ready steps, moves each to SCHEDULED,
// increments its attempt counter and stamps a fresh fencing token.
//
// Two halves of the contract matter to the dispatcher's capacity accounting,
// and internal/storetest asserts both:
//
//   - on error, NO leases are returned. Partial results are never returned.
//   - len(result) <= req.Limit, always. Returning fewer — including zero — is
//     normal and is not an error.
func (s *Store) Claim(ctx context.Context, req runmesh.ClaimRequest) ([]runmesh.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Limit < 1 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}

	var tools map[string]bool
	if len(req.Tools) > 0 {
		tools = make(map[string]bool, len(req.Tools))
		for _, t := range req.Tools {
			tools[t] = true
		}
	}

	var cands []candidate
	for _, j := range s.jobs {
		for _, step := range j.Steps {
			if claimable(j, step, req.Now, tools) {
				cands = append(cands, candidate{j, step})
			}
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// ORDER BY job.priority DESC, job.created_at, step.id — with the job id as
	// a final tie-break so the ordering is total and the test suite is
	// reproducible even when two jobs are created in the same instant.
	sort.Slice(cands, func(a, b int) bool {
		x, y := cands[a], cands[b]
		switch {
		case x.job.Priority != y.job.Priority:
			return x.job.Priority > y.job.Priority
		case !x.job.CreatedAt.Equal(y.job.CreatedAt):
			return x.job.CreatedAt.Before(y.job.CreatedAt)
		case x.job.ID != y.job.ID:
			return x.job.ID < y.job.ID
		default:
			return x.step.ID < y.step.ID
		}
	})
	if len(cands) > req.Limit {
		cands = cands[:req.Limit]
	}

	leases := make([]runmesh.Lease, 0, len(cands))
	for _, c := range cands {
		leases = append(leases, s.claimOneLocked(c.job, c.step, req))
	}
	return leases, nil
}

// claimOneLocked performs the SCHEDULED transition for a single step. The
// caller must hold s.mu.
func (s *Store) claimOneLocked(j *runmesh.Job, step *runmesh.Step, req runmesh.ClaimRequest) runmesh.Lease {
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

	s.appendEventLocked(j, stepEvent(runmesh.StepScheduled, step, req.Now))
	s.reconcileJobLocked(j, req.Now)

	return runmesh.Lease{
		ID:             step.LeaseID,
		Owner:          req.Owner,
		JobID:          j.ID,
		StepID:         step.ID,
		AttemptID:      runmesh.AttemptID(j.ID, step.ID, step.Attempt),
		IdempotencyKey: runmesh.IdempotencyKey(j.ID, step.ID),
		Attempt:        step.Attempt,
		Failures:       step.Failures,
		MaxAttempts:    step.MaxAttempts,
		Tool:           step.Tool,
		Params:         append(json.RawMessage(nil), step.Params...),
		Deps:           depResults(j, step),
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
