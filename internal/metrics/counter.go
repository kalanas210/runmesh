package metrics

import "sync/atomic"

// Counter is a monotonically increasing count on one atomic word.
//
// There is no mutex and no generation swap, because the write path is reached
// from settle() on a goroutine that is holding one of the worker pool's
// capacity tokens: a slow counter does not make metrics slow, it shrinks the
// worker pool. One atomic.Uint64 is the cheapest thing that is still correct
// under -race, and TestObserveIsAllocationFree asserts the claim rather than
// leaving it as prose.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n. It takes a uint64 rather than a float64 on purpose: every counter
// in this runtime counts events, and a float counter invites somebody to add
// 0.1 to something whose rate() is then meaningless.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value reads the counter.
//
// This is the in-package replacement for client_golang's testutil.ToFloat64,
// and it is cheaper than what it replaces: a test reads the typed value
// directly, with no gathering, no parsing and no dto round-trip. ADR 0012
// concedes testutil as the strongest argument for the library and this is the
// half of it that comes back for free.
func (c *Counter) Value() uint64 { return c.v.Load() }

func (c *Counter) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	return appendUintSample(dst, name, extra, c.v.Load())
}

func (c *Counter) series() int { return 1 }

// counterFunc is a counter whose value is read from an existing source at
// scrape time, so nothing is mirrored and nothing can drift from the source.
type counterFunc func() uint64

func (f counterFunc) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	return appendUintSample(dst, name, extra, f())
}

func (f counterFunc) series() int { return 1 }

// counterFloatFunc is the same thing over a source that is genuinely
// fractional, and it exists because forcing one through counterFunc is a bug
// that LOOKS like a working metric.
//
// Counter and counterFunc are integral on purpose — the note on Add says why:
// everything this runtime counts is events, and a float there invites somebody
// to add 0.1 to something whose rate() is then meaningless. But
// /cpu/classes/gc/total:cpu-seconds is not an event count, it is a duration,
// and it is a float64 that on a healthy process spends its entire life below 1.
// Bridged through a uint64 accessor it truncates to zero, for ever: the series
// exists, the TYPE line says counter, the help string promises CPU seconds, and
// every dashboard reads a flat line at zero and concludes the process has never
// collected garbage. That is worse than an absent series, for the reason stated
// at the top of runtimecol.go.
//
// The exposition TYPE is still `counter`, so rate() and increase() are
// unaffected — Prometheus has never required a counter's samples to be
// integers, only that they do not decrease. The only thing that changes is that
// the value is rendered through appendFloat instead of strconv.AppendUint.
type counterFloatFunc func() float64

func (f counterFloatFunc) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	return appendFloatSample(dst, name, extra, f())
}

func (f counterFloatFunc) series() int { return 1 }

// CounterVec is a counter with a closed label vocabulary.
type CounterVec struct{ v *vec }

// With resolves the label values to their counter.
//
// It NEVER returns nil, for any input. A value outside the declared vocabulary
// resolves to the shared "other" child and bumps
// runmesh_metrics_label_rejected_total; so does the wrong number of values. The
// call sites are engine goroutines, and a telemetry mistake must not be able to
// panic one.
func (cv *CounterVec) With(values ...string) *Counter {
	return cv.v.with(values).(*Counter)
}

// Rejected reports how many observations carried a label value outside this
// metric's vocabulary. It is what the cardinality tests assert on.
func (cv *CounterVec) Rejected() uint64 { return cv.v.rejected.Load() }
