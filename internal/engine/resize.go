package engine

import (
	"context"
	"sync"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// concurrencyAccumulator collects one interval's worth of AttemptSettled
// evidence for the adaptive controller.
//
// It is a small mutex-protected struct rather than a set of independent
// atomics, and that is deliberate: this is not the Observer (see observer.go
// for why THAT seam may neither lock nor allocate) — it is the engine's own
// internal bookkeeping, on the same footing as claimErrors and
// leasesExpired, called from settle at exactly the frequency inflightMu
// already is. Reading attempts, errors and latency out of three separate
// atomics could pair a count taken from one window with a sum taken from the
// next, which is a real inconsistency an uncontended mutex costs nothing to
// avoid.
type concurrencyAccumulator struct {
	mu       sync.Mutex
	attempts int
	errors   int
	latency  time.Duration
}

func (a *concurrencyAccumulator) record(isError bool, d time.Duration) {
	a.mu.Lock()
	a.attempts++
	if isError {
		a.errors++
	}
	a.latency += d
	a.mu.Unlock()
}

// snapshot reads and resets the window in one critical section, so the
// attempts, errors and latency it returns always describe exactly the same
// set of calls to record — never a count from after the next attempt already
// started landing.
func (a *concurrencyAccumulator) snapshot() ConcurrencyWindow {
	a.mu.Lock()
	w := ConcurrencyWindow{Attempts: a.attempts, Errors: a.errors, LatencySum: a.latency}
	a.attempts, a.errors, a.latency = 0, 0, 0
	a.mu.Unlock()
	return w
}

// concurrencySignal reports whether a decided attempt should influence the
// adaptive worker count and, when it should, whether it counts as an error.
//
// Discarded and Released outcomes are excluded. A Discard means another
// worker already owned the step, so the duration on it is how long THIS
// worker took to discover that, not how long any tool ran. A Release fires
// mostly during a drain (Classify's StopShutdown case), when every in-flight
// step reports at once — feeding that to the controller would look like a
// correlated mass failure at exactly the moment the pool is shutting down
// anyway, which is not evidence about whether it was sized right. Cancelled
// is excluded for a similar reason: an operator asked for that attempt to
// stop, which says nothing about capacity.
//
// Everything else that reaches settle — Succeeded, Failed, Retrying (this
// ATTEMPT failed, whether or not the step goes on to retry) and TimedOut —
// is exactly the set Classify sends to Store.Finish rather than to Discard or
// Release, so this switch and that one describe the same line.
func concurrencySignal(o AttemptOutcome) (isError, ok bool) {
	if o.Discarded || o.Released || o.State == runmesh.Cancelled {
		return false, false
	}
	switch o.State {
	case runmesh.Succeeded:
		return false, true
	case runmesh.Failed, runmesh.Retrying, runmesh.TimedOut:
		return true, true
	default:
		return false, false
	}
}

// recordConcurrency feeds one decided attempt to the adaptive controller's
// current window. Called once per settle, on the outcome value every exit
// path of settle already agreed on, so it can never disagree with what the
// Observer or the "step settled" log line reported for the same attempt.
func (e *Engine) recordConcurrency(o AttemptOutcome) {
	if isError, ok := concurrencySignal(o); ok {
		e.window.record(isError, o.Duration)
	}
}

// returnToken hands one capacity token back to the pool — UNLESS the
// controller has marked capacity for retirement, in which case this is the
// token that pays the debt, and circulation shrinks by one instead of
// staying flat.
//
// This is what makes a shrink gradual and safe: it never revokes a token a
// worker is actively holding while a step runs, only ones that are about to
// become free anyway, through the exact two paths that already returned a
// token before this feature existed — a worker finishing a lease
// (runWorker) and the dispatcher giving back capacity it did not use
// (giveBack). Both now call this instead of sending on idle directly.
//
// Lock-free: only retireDebt is contended, with a CAS loop exactly like the
// one resize uses to cancel debt. The idle send on a debt miss can never
// block — idle's buffer is MaxWorkers, resize never mints past size(), and
// size is always <= MaxWorkers — so this has the same "cannot block"
// property giveBack's own doc already claims for the fixed-pool case.
func (e *Engine) returnToken() {
	for {
		d := e.retireDebt.Load()
		if d <= 0 {
			e.idle <- struct{}{}
			return
		}
		if e.retireDebt.CompareAndSwap(d, d-1) {
			return
		}
	}
}

// resize moves the pool's target from whatever it is now to next. next is
// trusted as already decided and already clamped — resize is pure mechanism,
// the same division of labour Classify/settle has between deciding and
// carrying out.
//
// Growing mints tokens straight onto idle, after first cancelling any
// outstanding retirement debt: un-retiring a token nobody has swallowed yet
// is free, and cheaper than minting a new one that the very next return
// would just retire again. Shrinking never touches idle at all — it only
// adds to retireDebt, so the NEXT tokens to return (see returnToken) are the
// ones that pay for it, whenever that happens to be.
func (e *Engine) resize(next int) {
	cur := int(e.size.Swap(int64(next)))
	if next == cur {
		return
	}
	if next < cur {
		e.retireDebt.Add(int64(cur - next))
		return
	}

	grow := int64(next - cur)
	for grow > 0 {
		d := e.retireDebt.Load()
		if d <= 0 {
			break
		}
		take := min(grow, d)
		if !e.retireDebt.CompareAndSwap(d, d-take) {
			continue // lost a race with returnToken; re-read and retry
		}
		grow -= take
	}
	for range grow {
		e.idle <- struct{}{}
	}
}

// runConcurrency is the adaptive controller: once per Config.ConcurrencyInterval
// it reduces the interval's observations to a ConcurrencyWindow, asks the pure
// policy for the next target, and — only when that target actually moved —
// carries it out with resize and reports it.
//
// Start calls this only when MaxWorkers is wider than Workers — see its own
// comment for why an always-running ticker is both wasted work and a way to
// desync the fake clock's waiter count that several tests key their
// synchronisation on.
//
// baseline belongs to this goroutine alone: the lowest mean latency any
// non-empty window has recorded. It only ever falls, on purpose — a system
// that got slower and stayed slower should keep being compared against its
// best day, not quietly adopt the worse one as the new normal.
//
// Like the reconciler, it stops when ctx (the dispatcher's claim context) is
// cancelled: sizing decisions belong to steady-state operation, not to a
// drain already under way.
func (e *Engine) runConcurrency(ctx context.Context) {
	defer e.wgLoops.Done()
	tick := e.clock.NewTicker(e.cfg.ConcurrencyInterval)
	defer tick.Stop()

	var baseline time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			w := e.window.snapshot()
			if mean := w.Mean(); mean > 0 && (baseline == 0 || mean < baseline) {
				baseline = mean
			}
			cur := int(e.size.Load())
			next := e.concurrency.Next(cur, w, baseline)
			if next == cur {
				continue
			}
			e.resize(next)
			e.obs.ConcurrencyAdjusted(cur, next)
			e.log.Info("adaptive worker pool resized",
				"from", cur, "to", next, "max_workers", e.cfg.MaxWorkers,
				"window_attempts", w.Attempts, "window_errors", w.Errors,
				"window_mean_ms", w.Mean().Milliseconds(), "baseline_ms", baseline.Milliseconds())
		}
	}
}
