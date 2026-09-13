package engine_test

import (
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
)

func aimd() engine.Concurrency {
	return engine.Concurrency{Min: 2, Max: 16, ErrorRate: 0.2, Headroom: 0.5, Step: 1}
}

// TestConcurrencyGrowsOneStepWhenHealthy: a window with no errors and latency
// at or below baseline probes for one more worker, and no more than one — the
// additive half of AIMD.
func TestConcurrencyGrowsOneStepWhenHealthy(t *testing.T) {
	t.Parallel()
	c := aimd()
	w := engine.ConcurrencyWindow{Attempts: 50, Errors: 0, LatencySum: 50 * 100 * time.Millisecond}
	if got := c.Next(4, w, 100*time.Millisecond); got != 5 {
		t.Errorf("Next(4, healthy) = %d, want 5", got)
	}
}

// TestConcurrencyGrowthStopsAtMax: repeated healthy windows never push the
// pool past the configured ceiling.
func TestConcurrencyGrowthStopsAtMax(t *testing.T) {
	t.Parallel()
	c := aimd()
	w := engine.ConcurrencyWindow{Attempts: 10, LatencySum: 10 * 10 * time.Millisecond}
	current := c.Max - 1
	for range 5 {
		current = c.Next(current, w, 10*time.Millisecond)
	}
	if current != c.Max {
		t.Errorf("current settled at %d, want the ceiling %d", current, c.Max)
	}
}

// TestConcurrencyHalvesOnErrorRate is the multiplicative-decrease half: a
// window whose error rate meets the threshold cuts the pool immediately,
// rather than backing off one worker at a time the way growth adds one.
func TestConcurrencyHalvesOnErrorRate(t *testing.T) {
	t.Parallel()
	c := aimd()
	tests := []struct {
		current, errors, attempts, want int
	}{
		{current: 10, errors: 2, attempts: 10, want: 5}, // exactly at the 20% threshold
		{current: 10, errors: 5, attempts: 10, want: 5}, // well past it, still one halving
		{current: 11, errors: 3, attempts: 10, want: 6}, // rounds up
		{current: 3, errors: 1, attempts: 2, want: 2},   // floored at Min, not at 1
	}
	for _, tc := range tests {
		w := engine.ConcurrencyWindow{Attempts: tc.attempts, Errors: tc.errors}
		if got := c.Next(tc.current, w, 0); got != tc.want {
			t.Errorf("Next(%d, %d/%d errors) = %d, want %d",
				tc.current, tc.errors, tc.attempts, got, tc.want)
		}
	}
}

// TestConcurrencyNeverShrinksBelowMin: Min is the operator's own
// RUNMESH_WORKERS, chosen on purpose, and no run of bad windows erodes it.
func TestConcurrencyNeverShrinksBelowMin(t *testing.T) {
	t.Parallel()
	c := aimd()
	bad := engine.ConcurrencyWindow{Attempts: 10, Errors: 10}
	current := c.Min
	for range 6 {
		current = c.Next(current, bad, 0)
	}
	if current != c.Min {
		t.Errorf("current = %d after repeated failures, want it pinned at Min=%d", current, c.Min)
	}
}

// TestConcurrencyErrorRateOverridesLatency: a window can be both fast AND
// failing (a tool that fails instantly, say), and the error check must win —
// shrinking is the one decision that must never lose to "but it was quick."
func TestConcurrencyErrorRateOverridesLatency(t *testing.T) {
	t.Parallel()
	c := aimd()
	w := engine.ConcurrencyWindow{Attempts: 10, Errors: 10, LatencySum: 10 * time.Millisecond}
	if got := c.Next(10, w, 100*time.Millisecond); got != 5 {
		t.Errorf("Next with 100%% errors but low latency = %d, want the halved 5", got)
	}
}

// TestConcurrencyHoldsOnElevatedLatencyWithoutErrors: latency above baseline
// without any error pauses growth but does not itself shrink the pool — a
// slow window is not necessarily a failing one, and the whole point of
// Headroom is that it only stops digging.
func TestConcurrencyHoldsOnElevatedLatencyWithoutErrors(t *testing.T) {
	t.Parallel()
	c := aimd()
	w := engine.ConcurrencyWindow{Attempts: 20, Errors: 0, LatencySum: 20 * 200 * time.Millisecond}
	if got := c.Next(6, w, 100*time.Millisecond); got != 6 { // 200ms is +100% over a 100ms baseline
		t.Errorf("Next(6, 2x baseline latency, no errors) = %d, want 6 (hold)", got)
	}
}

// TestConcurrencyEmptyWindowHoldsCurrent: a window with no attempts is a pool
// with nothing to measure, not evidence of anything — it must change nothing,
// only clamp.
func TestConcurrencyEmptyWindowHoldsCurrent(t *testing.T) {
	t.Parallel()
	c := aimd()
	if got := c.Next(7, engine.ConcurrencyWindow{}, 50*time.Millisecond); got != 7 {
		t.Errorf("Next(7, empty window) = %d, want 7 unchanged", got)
	}
	// Still clamped: a current outside [Min, Max] — left over from a config
	// change, say — is corrected even when there is nothing else to decide.
	if got := c.Next(c.Max+5, engine.ConcurrencyWindow{}, 0); got != c.Max {
		t.Errorf("Next(Max+5, empty window) = %d, want clamped to Max=%d", got, c.Max)
	}
}

// TestConcurrencyZeroErrorRateDisablesShrink: the documented escape hatch —
// ErrorRate: 0 — must mean what it says, even at a 100% failure rate.
func TestConcurrencyZeroErrorRateDisablesShrink(t *testing.T) {
	t.Parallel()
	c := engine.Concurrency{Min: 1, Max: 10, ErrorRate: 0, Step: 1}
	w := engine.ConcurrencyWindow{Attempts: 5, Errors: 5}
	if got := c.Next(4, w, 0); got != 5 {
		t.Errorf("Next with ErrorRate=0 and 100%% failures = %d, want ordinary growth to 5", got)
	}
}

// TestConcurrencyMinEqualsMaxIsAConstant pins the default-configuration
// behaviour: RUNMESH_MAX_WORKERS defaults to RUNMESH_WORKERS, so Min == Max,
// and Next must return that one value regardless of what it observes — the
// fixed pool every existing deployment already runs.
func TestConcurrencyMinEqualsMaxIsAConstant(t *testing.T) {
	t.Parallel()
	c := engine.Concurrency{Min: 8, Max: 8, ErrorRate: 0.2, Headroom: 0.5, Step: 1}
	windows := []engine.ConcurrencyWindow{
		{},
		{Attempts: 100, Errors: 0, LatencySum: 100 * time.Millisecond},
		{Attempts: 100, Errors: 100, LatencySum: 100 * time.Millisecond},
	}
	for _, w := range windows {
		if got := c.Next(8, w, time.Millisecond); got != 8 {
			t.Errorf("Next(8, %+v) = %d with Min==Max==8, want 8", w, got)
		}
	}
}

// TestConcurrencyWindowMean.
func TestConcurrencyWindowMean(t *testing.T) {
	t.Parallel()
	if got := (engine.ConcurrencyWindow{}).Mean(); got != 0 {
		t.Errorf("Mean of an empty window = %s, want 0", got)
	}
	w := engine.ConcurrencyWindow{Attempts: 4, LatencySum: 2 * time.Second}
	if got := w.Mean(); got != 500*time.Millisecond {
		t.Errorf("Mean = %s, want 500ms", got)
	}
}
