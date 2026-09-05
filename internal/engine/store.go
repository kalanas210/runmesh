package engine

import (
	"context"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Store is declared HERE, by its consumer, rather than exported from the
// package that implements it.
//
// *memstore.Store satisfies it today and *pgstore.Store will satisfy it in
// Week 2 with no edit to this file. More importantly, the HTTP API declares a
// DIFFERENT, narrower interface over the same concrete store — so the API
// cannot claim, lease, heartbeat or finish anything, and that restriction is
// enforced by the compiler rather than by code review.
//
// Every method is one unit of work. No transaction object crosses this
// boundary, which leaves an implementation free to be a single statement, a
// small transaction, or eventually a remote service. Every timestamp is a
// parameter rather than a clock read, exactly as a SQL bind variable would be.
type Store interface {
	// Claim atomically selects up to req.Limit steps that are QUEUED with every
	// dependency SUCCEEDED, or RETRYING with their backoff elapsed; moves each
	// to SCHEDULED; increments Attempt; stamps a fresh lease; and appends
	// STEP_SCHEDULED. It is the shape of
	//
	//	WITH c AS (SELECT ... FOR UPDATE OF s SKIP LOCKED LIMIT $n)
	//	UPDATE job_steps ... FROM c ... RETURNING ...
	//
	// CONTRACT: on error it returns NO leases — partial results are never
	// returned — and len(result) <= req.Limit always. Returning fewer than
	// Limit, including zero, is normal and is not an error. The dispatcher's
	// capacity accounting depends on both halves, and storetest asserts them.
	Claim(ctx context.Context, req runmesh.ClaimRequest) ([]runmesh.Lease, error)

	// Start moves SCHEDULED -> RUNNING. It is a separate call because the gap
	// between "claimed" and "actually executing" becomes seconds in Week 3
	// (a pod pending) and the execution waterfall needs it as its own span.
	// ErrLeaseLost means the lease is stale: the worker drops the step and
	// writes nothing further.
	Start(ctx context.Context, l runmesh.Lease, now time.Time) error

	// Heartbeat extends the lease to `until` AND is the only channel by which a
	// running step learns it should stop. Fencing-guarded: a stale token
	// returns ErrLeaseLost.
	Heartbeat(ctx context.Context, l runmesh.Lease, now, until time.Time) (runmesh.Directive, error)

	// Finish applies the terminal (or RETRYING) transition, appends events and
	// recomputes the job rollup — atomically, in one critical section. Guarded
	// on the lease id AND the current state; anything else is ErrConflict
	// (Week 2: the UPDATE affected zero rows) and the worker discards its
	// result rather than retrying the write.
	Finish(ctx context.Context, o runmesh.Outcome) error

	// Release returns a step to QUEUED WITHOUT spending retry budget: the drain
	// deadline, or a lease voluntarily given up. Attempt is not decremented.
	Release(ctx context.Context, l runmesh.Lease, reason string, now time.Time) error

	// ExpireLeases is the crash-recovery sweep. Unlike Release it SPENDS
	// budget, because a worker that reliably dies on step X must eventually
	// exhaust MaxAttempts rather than crash-loop the whole fleet.
	ExpireLeases(ctx context.Context, now time.Time, limit int) ([]runmesh.Expired, error)

	// Ready is a LATENCY HINT, never a correctness requirement. The store does
	// a non-blocking send whenever something may have become claimable. A
	// PostgreSQL implementation may use LISTEN/NOTIFY or simply return nil —
	// and nil is safe, because a nil channel blocks for ever in select and the
	// dispatcher's ticker carries the load. Losing a hint costs latency; it
	// can never strand a job.
	Ready() <-chan struct{}
}
