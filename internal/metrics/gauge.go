package metrics

import "sync/atomic"

// Gauge is a value that goes up and down, held in one atomic.Int64.
//
// INTEGRAL, deliberately. Every gauge in this runtime counts things — workers,
// in-flight steps, idle capacity, queue depth — so there is nothing to
// represent between two integers, and an int64 avoids the math.Float64bits
// compare-and-swap dance a float gauge needs. A future gauge that genuinely
// measures a ratio should be a GaugeFunc reading a float, not a reason to make
// every gauge in the process pay for a CAS loop.
type Gauge struct{ v atomic.Int64 }

// Set replaces the value.
func (g *Gauge) Set(n int64) { g.v.Store(n) }

// Add moves the value by n, which may be negative.
func (g *Gauge) Add(n int64) { g.v.Add(n) }

// Value reads the gauge. Like Counter.Value, this is what a test asserts on
// instead of gathering and parsing.
func (g *Gauge) Value() int64 { return g.v.Load() }

func (g *Gauge) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	return appendIntSample(dst, name, extra, g.v.Load())
}

func (g *Gauge) series() int { return 1 }

// gaugeFunc is a gauge read from an existing source at scrape time.
//
// This is how every runtime gauge in this binary is exported, and the reason is
// the drift argument written out in gauges.go: engine.Stats() and a mirrored
// copy WILL disagree the first time somebody adds an early return on one path
// and not the other. A value read from its source at render time cannot.
type gaugeFunc func() float64

func (f gaugeFunc) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	return appendFloatSample(dst, name, extra, f())
}

func (f gaugeFunc) series() int { return 1 }

// GaugeVec is a gauge with a closed label vocabulary. The one use in this
// binary is runmesh_build_info, whose vocabulary is a single fixed version
// string — the degenerate but entirely real case of a closed set.
type GaugeVec struct{ v *vec }

// With resolves the label values to their gauge. Like CounterVec.With it never
// returns nil and never panics.
func (gv *GaugeVec) With(values ...string) *Gauge {
	return gv.v.with(values).(*Gauge)
}
