package metrics

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// Histogram is an explicit-bucket histogram on atomics.
//
// Three invariants are worth stating, because each of them is a bug this shape
// makes impossible rather than merely avoids:
//
// (a) _count IS NEVER STORED. It is emitted as the le="+Inf" cumulative
// bucket, so the two cannot disagree by construction. A histogram without an
// +Inf bucket is invalid, and a histogram whose +Inf bucket differs from its
// _count is worse than invalid — it parses, and every quantile computed from it
// is quietly wrong. client_golang has to maintain that agreement; this design
// gets it for free by having only one number.
//
// (b) Observe TAKES NO LOCK AND ALLOCATES NOTHING. The scan over the bounds is
// linear, not binary: at twelve float64s a linear scan is faster and far more
// branch-predictable than a binary search, and it fits in a cache line's worth
// of contiguous memory. One CAS loop lands the sum and one atomic add lands the
// bucket. This is not micro-optimisation for its own sake — Observe is reached
// from settle() on a goroutine that is holding a worker pool capacity token, so
// a slow implementation does not make metrics slow, it shrinks the pool.
// TestObserveIsAllocationFree pins the zero-allocation half with
// testing.AllocsPerRun.
//
// (c) _sum AND _count CAN SKEW BY ONE OBSERVATION within a scrape instant,
// because the write path is two independent atomics rather than a hot/cold
// generation swap. That is real and it is accepted deliberately: the writes are
// ordered so the sum lands FIRST, which makes _count the conservative one, and
// the only way to remove the skew is a mutex on the settle path. Observability
// must never apply backpressure to execution (the rule is written out at
// internal/memstore/subscribe.go), so that mutex is not available. Do not let
// anyone later "fix" this.
type Histogram struct {
	// bounds are the INCLUSIVE upper edges, ascending. Fixed at construction and
	// never written again, so it needs no synchronisation.
	bounds []float64
	// counts holds PER-BUCKET counts, not cumulative ones, with one more entry
	// than there are bounds for the +Inf overflow. The running sum happens at
	// render. Storing cumulative counts would mean every Observe writing to
	// every bucket at or above the match, which is the other classic
	// hand-rolled histogram bug and is O(buckets) on the hot path.
	counts  []atomic.Uint64
	sumBits atomic.Uint64
	// dropped counts NaN observations. A NaN in _sum poisons the family until
	// the process restarts — every quantile and every rate over it is NaN, and
	// there is no way to un-poison a counter — so a NaN is refused rather than
	// added. It is counted so the refusal is visible to a test; it is
	// deliberately not its own series, because a NaN observation is a
	// programming error that a test catches, not an operational condition an
	// SRE pages on.
	dropped atomic.Uint64
}

func newHistogram(name string, bounds []float64) *Histogram {
	checkBounds(name, bounds)
	return &Histogram{
		bounds: bounds,
		counts: make([]atomic.Uint64, len(bounds)+1),
	}
}

// checkBounds rejects a bucket slice that is not strictly ascending, and any
// non-finite bound.
//
// Like every other constructor check in this package this PANICS rather than
// returning an error, because it can only be a mistake in run()'s wiring: a
// descending or duplicated bound makes the cumulative sum at render produce
// bucket counts that decrease, which Prometheus accepts and
// histogram_quantile then interpolates into nonsense. Failing the boot is the
// only outcome that cannot be ignored.
func checkBounds(name string, bounds []float64) {
	if len(bounds) == 0 {
		panic(fmt.Sprintf("metrics: histogram %q has no bucket bounds", name))
	}
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) {
			panic(fmt.Sprintf("metrics: histogram %q has a non-finite bound at index %d; "+
				"the +Inf bucket is implicit and must not be declared", name, i))
		}
		if i > 0 && b <= bounds[i-1] {
			panic(fmt.Sprintf("metrics: histogram %q bounds are not strictly ascending "+
				"(%v then %v); cumulative bucket counts would decrease", name, bounds[i-1], b))
		}
	}
}

// Observe records one value.
func (h *Histogram) Observe(v float64) {
	if math.IsNaN(v) {
		h.dropped.Add(1)
		return
	}
	// Linear scan. A negative value lands in bucket 0 by construction, since
	// every bound in this package is positive and v <= bounds[0] holds — which
	// is the correct answer for a duration measured across a clock the caller
	// read in the wrong order, rather than an underflow nobody would notice.
	i := 0
	for i < len(h.bounds) && v > h.bounds[i] {
		i++
	}

	// The sum lands before the count. A reader between the two sees a sum that
	// includes this observation and a count that does not, so _count is the
	// conservative one — see invariant (c) on the type.
	for {
		old := h.sumBits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if h.sumBits.CompareAndSwap(old, next) {
			break
		}
	}
	h.counts[i].Add(1)
}

// ObserveDuration is the only form the engine uses.
//
// Having it here means the seconds conversion lives in exactly ONE place, and
// therefore that no call site can accidentally export milliseconds under a
// name ending in _seconds. That mistake is invisible — the series is present,
// the graph is smooth, and every threshold on it is wrong by a factor of a
// thousand — which is precisely why it is worth removing structurally rather
// than by review.
func (h *Histogram) ObserveDuration(d time.Duration) { h.Observe(d.Seconds()) }

// Count reports how many observations have landed. Like Counter.Value it is the
// in-package replacement for testutil, and it reads the SAME number the +Inf
// bucket renders, because there is only one.
func (h *Histogram) Count() uint64 {
	var n uint64
	for i := range h.counts {
		n += h.counts[i].Load()
	}
	return n
}

// Sum reports the running total of every observation.
func (h *Histogram) Sum() float64 { return math.Float64frombits(h.sumBits.Load()) }

// Dropped reports how many NaN observations were refused. See the field comment.
func (h *Histogram) Dropped() uint64 { return h.dropped.Load() }

// appendSamples renders the bucket, sum and count lines.
//
// The cumulative running sum happens HERE, ascending, and the +Inf bucket is
// reused verbatim as _count. That ordering is the whole of invariant (a).
func (h *Histogram) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	pairs := make([]labelPair, len(extra), len(extra)+1)
	copy(pairs, extra)
	pairs = append(pairs, labelPair{name: "le"})

	var cum uint64
	for i, b := range h.bounds {
		cum += h.counts[i].Load()
		pairs[len(pairs)-1].value = formatBound(b)
		dst = appendUintSample(dst, name+"_bucket", pairs, cum)
	}
	cum += h.counts[len(h.bounds)].Load()
	pairs[len(pairs)-1].value = posInf
	dst = appendUintSample(dst, name+"_bucket", pairs, cum)

	dst = appendFloatSample(dst, name+"_sum", extra, h.Sum())
	dst = appendUintSample(dst, name+"_count", extra, cum)
	return dst
}

// series counts one per bucket, plus the implicit +Inf, plus _sum and _count.
func (h *Histogram) series() int { return len(h.bounds) + 3 }

// HistogramVec is a histogram with a closed label vocabulary. Every child
// shares the same bounds, which is what makes summing bucket counts across the
// label dimension a legal thing for a Prometheus query to do.
type HistogramVec struct{ v *vec }

// With resolves the label values to their histogram. Like CounterVec.With it
// never returns nil and never panics.
func (hv *HistogramVec) With(values ...string) *Histogram {
	return hv.v.with(values).(*Histogram)
}
