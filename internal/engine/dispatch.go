package engine

import (
	"context"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// runDispatcher is the single goroutine that turns free worker capacity into
// claimed leases.
//
// The invariant it maintains is the whole of the engine's backpressure story:
// it never claims a step it does not already hold an idle token for. That is
// why sending on the unbuffered lease channel can never block indefinitely —
// a token IS a worker that will receive — and why the store is never asked for
// more work than the pool can start.
//
// Deadlock argument, in full. The dispatcher blocks only on <-idle and on
// leases <-, and both are in a select with ctx.Done(). Workers block only on
// range leases (released by close) and on idle <- (a buffer of Workers with at
// most Workers tokens in existence, so it never blocks). Every store call
// carries a bounded context. There is no cycle in the wait-for graph, and the
// HTTP handlers are not in the graph at all: they write to the store and
// return.
func (e *Engine) runDispatcher(ctx context.Context) {
	defer close(e.leases) // the dispatcher is the ONLY closer of this channel
	tick := e.clock.NewTicker(e.cfg.PollInterval)
	defer tick.Stop()

	for {
		held, ok := e.acquire(ctx)
		if !ok {
			return
		}

		cctx, cancel := clock.WithWriteDeadline(ctx, e.cfg.StoreTimeout)
		leases, err := e.store.Claim(cctx, runmesh.ClaimRequest{
			Owner:    e.cfg.Owner,
			Limit:    held,
			LeaseTTL: e.cfg.LeaseTTL,
			Now:      e.clock.Now(),
			Tools:    e.cfg.Tools,
		})
		cancel()

		if err != nil {
			// Claim's contract says an error means NO leases. Returning every
			// token unconditionally is what keeps pool capacity from leaking
			// away one failed claim at a time.
			e.giveBack(held)
			e.claimErrors.Add(1)
			if ctx.Err() == nil {
				e.log.Error("claim failed", "err", err)
			}
			if !e.park(ctx, tick) {
				return
			}
			continue
		}

		if len(leases) > held {
			// A misbehaving store must not be able to oversubscribe the pool.
			// The excess steps are handed straight back rather than dropped,
			// so they stay claimable instead of sitting leased and idle.
			e.log.Error("store returned more leases than requested",
				"got", len(leases), "want", held)
			e.releaseLeases(ctx, leases[held:], "oversubscribed")
			leases = leases[:held]
		}

		handed := 0
		for _, l := range leases {
			select {
			case e.leases <- l:
				handed++
			case <-ctx.Done():
				e.giveBack(held - handed)
				// Give the STEPS back too, not just the tokens: a lease we
				// claimed but never handed to a worker would otherwise sit
				// until it expired, and expiry spends retry budget that this
				// shutdown did not deserve to spend.
				e.releaseLeases(ctx, leases[handed:], runmesh.CodeShutdown)
				return
			}
		}

		e.giveBack(held - handed)
		if handed == 0 {
			if !e.park(ctx, tick) {
				return
			}
		}
	}
}

// acquire blocks for at least one idle token, then greedily drains up to
// ClaimBatch of them so a busy queue is claimed in batches rather than one
// round trip per step.
func (e *Engine) acquire(ctx context.Context) (int, bool) {
	select {
	case <-e.idle:
	case <-ctx.Done():
		return 0, false
	}
	n := 1
	for n < e.cfg.ClaimBatch {
		select {
		case <-e.idle:
			n++
		default:
			return n, true
		}
	}
	return n, true
}

// giveBack returns unused capacity. It can never block: the channel's buffer
// is Workers and at most Workers tokens exist.
func (e *Engine) giveBack(n int) {
	for range n {
		e.idle <- struct{}{}
	}
}

// releaseLeases hands claimed-but-unstarted steps back to the queue. It writes
// on a detached context because the usual reason for calling it is that ctx
// has just been cancelled.
func (e *Engine) releaseLeases(ctx context.Context, leases []runmesh.Lease, reason string) {
	for _, l := range leases {
		wctx, cancel := clock.WithWriteDeadline(ctx, e.cfg.StoreTimeout)
		if err := e.store.Release(wctx, l, reason, e.clock.Now()); err != nil {
			e.log.Warn("could not release an unstarted lease",
				"attempt_id", l.AttemptID, "reason", reason, "err", err)
		}
		cancel()
	}
}

// park waits for the store's readiness hint or the poll ticker, whichever
// comes first. A missed hint costs one tick of latency; the ticker is the
// backstop that makes the hint optional, which is what lets a PostgreSQL store
// return a nil channel until LISTEN/NOTIFY exists.
func (e *Engine) park(ctx context.Context, tick clock.Ticker) bool {
	select {
	case <-ctx.Done():
		return false
	case <-e.store.Ready():
		return true
	case <-tick.C():
		return true
	}
}
