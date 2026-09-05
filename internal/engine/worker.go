package engine

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// toolResult is what the tool runner goroutine hands back. The channel it
// travels on is buffered, so a runner that outlives its worker can always send
// and exit rather than leaking.
type toolResult struct {
	out tools.Output
	err error
}

// runWorker consumes leases until the dispatcher closes the channel.
//
// Exactly one idle token is returned per lease consumed, on every path
// including a panic. That accounting is what bounds concurrency: the number of
// tokens in flight is the number of steps that may be executing.
func (e *Engine) runWorker(hard context.Context, id int) {
	defer e.wg.Done()
	log := e.log.With("worker", id)
	for l := range e.leases {
		e.guarded(hard, log, l)
		e.idle <- struct{}{}
	}
}

// guarded is the engine's own panic barrier. A bug in RunMesh must shrink
// neither the worker pool nor the process: the step keeps its lease, the
// reconciler reclaims it when that lease expires, and the pool carries on.
func (e *Engine) guarded(hard context.Context, log *slog.Logger, l runmesh.Lease) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("engine panic while executing a step",
				"attempt_id", l.AttemptID, "tool", l.Tool,
				"panic", r, "stack", string(debug.Stack()))
		}
	}()
	e.execute(hard, log, l)
}

// execute runs one attempt.
//
// The single select below carries all four ways a step can stop — the tool
// returning, its deadline firing, a cancellation arriving on a heartbeat, and
// the lease being lost — because keeping them in one place is what makes their
// interactions reviewable. Splitting them across goroutines is how a cancelled
// step ends up classified as a retryable failure.
func (e *Engine) execute(hard context.Context, log *slog.Logger, l runmesh.Lease) {
	log = log.With("job_id", l.JobID, "step_id", l.StepID,
		"attempt", l.Attempt, "tool", l.Tool)

	started := e.clock.Now()
	if err := e.storeStart(hard, l, started); err != nil {
		// ErrLeaseLost / ErrConflict: somebody else owns this step, so we drop
		// it silently. Anything else is transient — the lease will expire and
		// the reconciler will requeue it, which is the same path a crashed
		// worker takes, and therefore already tested.
		if !errors.Is(err, runmesh.ErrLeaseLost) && !errors.Is(err, runmesh.ErrConflict) {
			log.Warn("could not mark step running", "err", err)
		}
		return
	}

	runCtx, cancel := e.clock.WithTimeout(hard, l.Timeout)
	defer cancel()

	done := make(chan toolResult, 1)
	e.track(l)
	defer e.untrack(l)
	go e.invoke(runCtx, log, l, done)

	hb := e.clock.NewTicker(e.cfg.HeartbeatInterval)
	defer hb.Stop()

	// Nilling a channel disables its select branch. Both uses below matter:
	// hbC stops renewing a lease we have discovered we no longer own, and
	// expired makes an always-ready closed channel fire exactly once.
	hbC := hb.C()
	expired := runCtx.Done()

	stop := StopNone
	var reason runmesh.CancelReason
	var abandon <-chan time.Time

	// armAbandon starts the clock on a tool that has been told to stop. It is
	// idempotent BY DESIGN: the cancel flag is sticky, so every subsequent
	// heartbeat reports the same cancellation, and re-arming here would push
	// the deadline out by one heartbeat interval every heartbeat interval. With
	// the shipped defaults (heartbeat 5s, grace 10s) that timer would never
	// fire at all, and a tool ignoring its context would hold a worker for the
	// life of the process.
	armAbandon := func() {
		if abandon == nil {
			abandon = e.clock.After(e.cfg.AbandonGrace)
		}
	}

	// resolveStop names the reason a step stopped when the context ended but
	// no branch has recorded why yet. It asks the PARENT, never runCtx.Err():
	// the step deadline and the drain deadline are different contexts, and
	// confusing them turns a deploy into a wave of spurious timeouts that each
	// burn a retry.
	resolveStop := func() Stop {
		if hard.Err() != nil {
			return StopShutdown
		}
		return StopTimeout
	}

	for {
		select {
		case r := <-done:
			// A ready select case is chosen at random, so the deadline firing
			// and the tool returning at the same instant can hand us the result
			// before the <-expired branch ever runs. Without this, a step that
			// genuinely timed out would be classified from the tool's own
			// context error as tool_broke_contract, and never retried.
			if stop == StopNone && runCtx.Err() != nil {
				stop = resolveStop()
			}
			e.settle(hard, log, l, stop, reason, r, started)
			return

		case <-hbC:
			now := e.clock.Now()
			hctx, hcancel := clock.WithWriteDeadline(hard, e.cfg.StoreTimeout)
			dir, err := e.store.Heartbeat(hctx, l, now, now.Add(e.cfg.LeaseTTL))
			hcancel()
			switch {
			case errors.Is(err, runmesh.ErrLeaseLost), errors.Is(err, runmesh.ErrConflict):
				// Losing the lease overrides any earlier reason: somebody else
				// owns this step now, so whatever we were about to write is
				// void. ORDER MATTERS — stop is assigned before cancel() is
				// called, so the classifier can never see a cancelled context
				// without knowing why it was cancelled.
				stop = StopLost
				hbC = nil // stop renewing a lease we do not own
				armAbandon()
				log.Warn("lease lost; abandoning this attempt", "err", err)
				cancel()
			case err == nil && dir.Cancel && stop == StopNone:
				// Only the FIRST cancellation decides anything. Renewal keeps
				// running so the lease is held while the grace elapses.
				stop, reason = StopCancel, dir.Reason
				armAbandon()
				log.Info("cancellation received", "reason", string(dir.Reason))
				cancel()
			case err != nil:
				// A transient heartbeat failure is not fatal: the lease still
				// has most of its TTL left and the next tick will renew it.
				log.Warn("heartbeat failed", "err", err)
			}

		case <-expired:
			expired = nil // a closed channel is always ready: fire this once
			if stop == StopNone {
				stop = resolveStop()
			}
			armAbandon()

		case <-abandon:
			// The tool is ignoring its context. We settle without it and let
			// its goroutine finish into the buffered channel whenever it
			// pleases; waiting for ever would park a worker permanently and
			// eventually deadlock the pool.
			log.Error("tool ignored cancellation; settling without it",
				"stop", stop.String(), "grace", e.cfg.AbandonGrace)
			e.settle(hard, log, l, stop, reason,
				toolResult{err: runmesh.Fatal(runmesh.CodeAbandoned, "tool ignored cancellation")},
				started)
			return
		}
	}
}

// invoke runs the executor on its own goroutine, behind the second panic
// barrier. This one is the backstop that guarantees `done` is always written
// even if the executor layer itself panics — without it a worker could park
// for ever waiting for a result that will never arrive.
func (e *Engine) invoke(ctx context.Context, log *slog.Logger, l runmesh.Lease, done chan<- toolResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("executor panic", "attempt_id", l.AttemptID,
				"panic", r, "stack", string(debug.Stack()))
			done <- toolResult{err: runmesh.Fatal(runmesh.CodeEnginePanic, "%v", r)}
		}
	}()

	out, err := e.exec.Execute(ctx, tools.Input{
		JobID:          l.JobID,
		StepID:         l.StepID,
		Attempt:        l.Attempt,
		AttemptID:      l.AttemptID,
		IdempotencyKey: l.IdempotencyKey,
		Tool:           l.Tool,
		Params:         l.Params,
		Deps:           l.Deps,
		Limits: tools.Limits{
			Timeout:        l.Timeout,
			MaxAttempts:    l.MaxAttempts,
			MaxOutputBytes: e.cfg.MaxOutputBytes,
		},
		Log:   log,
		Clock: e.clock,
	})
	done <- toolResult{out: out, err: err}
}

// settle is the ONLY place an outcome is written, and it always writes on a
// context detached from the one the step was running under.
//
// Using runCtx here — the context that was just cancelled in order to stop the
// step — is the bug that leaves cancelled jobs stuck in RUNNING for ever: the
// write to record the cancellation fails because of the cancellation.
// clock.WithWriteDeadline encodes context.WithoutCancel so no call site can
// forget it, and the clock purity test stops anyone hand-building the wrong
// thing instead.
func (e *Engine) settle(hard context.Context, log *slog.Logger, l runmesh.Lease,
	stop Stop, reason runmesh.CancelReason, r toolResult, started time.Time) {

	ended := e.clock.Now()
	d := Classify(stop, r.err, l.Failures, l.MaxAttempts, e.cfg.Backoff)
	if d.Error != nil && reason != "" {
		d.Error.Message = "cancelled: " + string(reason)
	}
	if d.Discard {
		log.Info("outcome discarded; another worker owns this step")
		return
	}

	ctx, cancel := clock.WithWriteDeadline(hard, e.cfg.StoreTimeout)
	defer cancel()

	if d.Release {
		if err := e.store.Release(ctx, l, d.Reason, ended); err != nil &&
			!errors.Is(err, runmesh.ErrLeaseLost) && !errors.Is(err, runmesh.ErrConflict) {
			log.Error("could not release step", "err", err)
		}
		return
	}

	out := runmesh.Outcome{
		Lease:     l,
		State:     d.State,
		Result:    r.out.Result,
		Error:     d.Error,
		CountFail: d.CountFail,
		StartedAt: started,
		EndedAt:   ended,
	}
	if d.State == runmesh.Retrying {
		out.NextAttemptAt = ended.Add(d.RetryIn)
	}
	if err := e.store.Finish(ctx, out); err != nil {
		// ErrLeaseLost and ErrConflict are expected: the step was reclaimed
		// while we were running it, and the new holder owns the outcome. Any
		// other error means the outcome is lost and the lease will expire, so
		// it is worth an ERROR line.
		if errors.Is(err, runmesh.ErrLeaseLost) || errors.Is(err, runmesh.ErrConflict) {
			log.Info("outcome rejected; step was reclaimed", "err", err)
			return
		}
		log.Error("could not persist outcome", "state", d.State.String(), "err", err)
		return
	}
	log.Debug("step settled", "state", d.State.String(),
		"duration_ms", ended.Sub(started).Milliseconds())
}

func (e *Engine) storeStart(hard context.Context, l runmesh.Lease, at time.Time) error {
	ctx, cancel := clock.WithWriteDeadline(hard, e.cfg.StoreTimeout)
	defer cancel()
	return e.store.Start(ctx, l, at)
}
