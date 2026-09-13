package engine

import "time"

// Concurrency is the pure policy that turns one interval's worth of observed
// attempts into the worker pool's next target size. It is AIMD — additive
// increase, multiplicative decrease — the shape TCP congestion control uses,
// for the same reason: a pool that is healthy only needs to be FOUND, one
// worker at a time, and a pool that is failing needs to shed load NOW, not
// after N more probes have each cost an attempt. Growing slowly and shrinking
// fast is asymmetric on purpose.
//
// It has no clock, holds nothing shared, and calling it twice with the same
// arguments always gives the same answer — the same property Backoff and
// Classify have, and for the same reason: the decision belongs in one place
// that can be table-tested, and the goroutine that keeps a window and calls
// this on a tick (see resize.go) should have nothing left to get wrong.
type Concurrency struct {
	// Min and Max bound every answer Next ever gives. In production Min is
	// Config.Workers — the floor an operator chose on purpose, which adaptive
	// sizing may not eat into — and Max is Config.MaxWorkers. Min == Max makes
	// Next constant, which is exactly the fixed pool every deployment that
	// never sets RUNMESH_MAX_WORKERS still gets.
	Min, Max int

	// ErrorRate is the fraction of a window's attempts that must have carried
	// an error — State Failed, Retrying or TimedOut, i.e. this ATTEMPT did not
	// succeed, whether or not the step goes on to retry — to halve the pool.
	// Zero disables error-based shrinking; that is never the right production
	// setting, but it is what keeps the growth-only test cases below from
	// having to fake a healthy error rate too.
	ErrorRate float64

	// Headroom is how far a window's mean latency may sit above the best mean
	// any window has recorded so far before growth pauses. 0.5 permits a 50%
	// rise over baseline. Crossing it does not shrink the pool — a slow window
	// is not necessarily a failing one, and a queueing delay that is really
	// the tool being slow today would make the controller shrink the very
	// capacity that could work it off — it only stops digging.
	Headroom float64

	// Step is how many workers one healthy window adds.
	Step int
}

// ConcurrencyWindow summarises one interval's worth of AttemptSettled
// observations — already reduced, never raw samples, which is what keeps
// Next pure and leaves the running baseline as the only state anyone has to
// carry between calls (see resize.go's runConcurrency).
type ConcurrencyWindow struct {
	Attempts   int
	Errors     int
	LatencySum time.Duration
}

// Mean is the window's mean attempt latency, or zero for a window that saw no
// attempts.
func (w ConcurrencyWindow) Mean() time.Duration {
	if w.Attempts == 0 {
		return 0
	}
	return w.LatencySum / time.Duration(w.Attempts)
}

// Next returns the worker count for the interval AFTER w, given the count
// DURING w and the lowest mean latency any window has recorded so far. The
// baseline is a plain argument rather than something Next tracks itself,
// because it has to survive across calls and Next must hold no state to stay
// pure — the caller's running minimum is the only sensible owner of it.
//
// An empty window returns current, clamped, and decides nothing else: a pool
// with nothing to measure has no evidence to act on, and shrinking it on
// silence would punish an idle system rather than a struggling one.
func (c Concurrency) Next(current int, w ConcurrencyWindow, baseline time.Duration) int {
	current = c.clamp(current)
	if w.Attempts == 0 {
		return current
	}
	if c.ErrorRate > 0 && float64(w.Errors)/float64(w.Attempts) >= c.ErrorRate {
		// Rounds UP so a pool of 1 stays at 1 instead of being floored to 0 by
		// integer division, and so Min is reachable in one shrink from Min+1
		// rather than orbiting it forever.
		return c.clamp((current + 1) / 2)
	}
	if baseline > 0 && w.Mean() > baseline+time.Duration(float64(baseline)*c.Headroom) {
		return current // hold: latency is elevated, but nothing has failed yet
	}
	return c.clamp(current + c.Step)
}

func (c Concurrency) clamp(n int) int {
	if n < c.Min {
		return c.Min
	}
	if n > c.Max {
		return c.Max
	}
	return n
}
