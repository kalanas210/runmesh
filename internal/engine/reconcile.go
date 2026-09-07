package engine

import (
	"context"

	"github.com/kalanas210/runmesh/internal/clock"
)

// runReconciler is the loop that makes a killed worker recoverable.
//
// Retries handle a step that failed. Leases and this reconciler handle a
// WORKER that failed: without it, a process killed mid-step leaves that step
// in SCHEDULED or RUNNING for ever, with a lease nobody will ever renew, and
// the job never finishes. That is the difference between a runtime that claims
// fault tolerance and one that has it.
//
// It also runs once at startup, before the dispatcher begins, so a restart
// reclaims whatever the previous process was holding. Against the in-memory
// store that sweep finds nothing, because the state died with the process;
// against PostgreSQL it is the whole of crash recovery, and
// cmd/server/crash_test.go kills a real server mid-step to prove it.
func (e *Engine) runReconciler(ctx context.Context) {
	defer e.wgLoops.Done()
	tick := e.clock.NewTicker(e.cfg.ReconcileInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			e.Reconcile(ctx)
		}
	}
}

// Reconcile performs one expired-lease sweep and reports how many steps it
// reclaimed. It is exported so the boot-time sweep and the tests can drive it
// directly, with no goroutine and no clock advancing involved.
func (e *Engine) Reconcile(ctx context.Context) int {
	rctx, cancel := clock.WithWriteDeadline(ctx, e.cfg.StoreTimeout)
	defer cancel()

	expired, err := e.store.ExpireLeases(rctx, e.clock.Now(), e.cfg.ReconcileBatch)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("lease sweep failed", "err", err)
		}
		return 0
	}
	for _, x := range expired {
		e.log.Warn("reclaimed an expired lease",
			"job_id", x.JobID, "step_id", x.StepID, "attempt", x.Attempt,
			"previous_owner", x.Owner, "new_state", x.NewState.String())
	}
	if n := len(expired); n > 0 {
		e.leasesExpired.Add(uint64(n))
		return n
	}
	return 0
}
