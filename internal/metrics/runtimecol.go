package metrics

import (
	"fmt"
	"math"
	"runtime"
	rtmetrics "runtime/metrics"
	"sync"
	"time"
)

// The Go runtime bridge.
//
// WHAT IS DELIBERATELY NOT HERE, and why. Nothing in this file is named
// process_* except process_start_time_seconds. client_golang's process
// collector reads /proc/self/{stat,status,fd} through prometheus/procfs to
// produce process_cpu_seconds_total, process_resident_memory_bytes and
// process_open_fds. That is roughly sixty lines to recover on Linux, it is a
// no-op on Windows and macOS in client_golang too, and under Kubernetes
// cAdvisor already publishes container_cpu_usage_seconds_total and
// container_memory_working_set_bytes — which are better numbers and are what an
// SRE actually alerts on.
//
// What is NOT acceptable is approximating them from runtime/metrics and keeping
// the names. /memory/classes/total:bytes is the Go runtime's own mapped memory
// and is NOT RSS; /cpu/classes/total:cpu-seconds is not process CPU. A series
// named after something it is not is worse than an absent series, because the
// absent one produces a blank panel and the misnamed one produces a confident
// wrong answer. If the real thing is ever needed it should be RECOVERED from
// /proc, not faked from here.
//
// process_start_time_seconds is the exception because it is exact, needs
// nothing but a clock, and is the series Prometheus itself uses to detect a
// restart.

// runtimeNames is the fixed allowlist. Reading a fixed slice rather than
// metrics.All() is what keeps the series count a constant: Go adds metrics
// between releases, and a collector that exports whatever it finds would change
// this binary's cardinality on a toolchain upgrade.
var runtimeNames = []string{
	"/gc/cycles/total:gc-cycles",
	"/gc/heap/allocs:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/total:bytes",
	"/cpu/classes/gc/total:cpu-seconds",
	"/sched/latencies:seconds",
}

// RegisterRuntime adds the Go runtime metrics to r.
//
// Every one is a CounterFunc, a GaugeFunc or a pull-based collector, so the
// registry still owns no goroutine — see the note in gauges.go about the
// engine's fixed goroutine census.
func RegisterRuntime(r *Registry) {
	// Asserted at boot for the same reason register() panics on a bad name: a
	// bucket slice that outgrew the fixed fold array would otherwise be an
	// out-of-range panic on the first scrape of a server that had already
	// started serving work. See schedBucketCap.
	if len(bucketsSched)+1 > schedBucketCap {
		panic(fmt.Sprintf("metrics: bucketsSched has %d bounds, which needs %d fold "+
			"slots against a schedBucketCap of %d; raise the constant",
			len(bucketsSched), len(bucketsSched)+1, schedBucketCap))
	}

	src := newRuntimeSource(r)
	start := float64(r.clk.Now().UnixNano()) / 1e9

	// go_goroutines matters more in THIS process than it would in most, because
	// the engine's package doc fixes the census: exactly 2 + Workers, plus one
	// transient goroutine per in-flight step. So this gauge is not background
	// colour — it is a live assertion of a stated design invariant, and a value
	// that drifts above workers + inflight + 2 (plus net/http's own, which is
	// why it is not an equality) is a leak with a specific place to look.
	r.GaugeFunc(Opts{
		Name: "go_goroutines",
		Help: "Goroutines that currently exist. The engine fixes its own census at 2 + workers + one per in-flight step.",
	}, func() float64 { return float64(runtime.NumGoroutine()) })

	r.GaugeFunc(Opts{
		Name: "process_start_time_seconds",
		Help: "Unix time at which this process started, for restart detection.",
	}, func() float64 { return start })

	r.CounterFunc(Opts{
		Name: "go_gc_cycles_total",
		Help: "Completed garbage collection cycles.",
	}, func() uint64 { return src.uint("/gc/cycles/total:gc-cycles") })

	r.CounterFunc(Opts{
		Name: "go_gc_heap_allocs_bytes_total",
		Help: "Cumulative bytes allocated on the heap.",
	}, func() uint64 { return src.uint("/gc/heap/allocs:bytes") })

	r.GaugeFunc(Opts{
		Name: "go_memory_heap_objects_bytes",
		Help: "Bytes currently occupied by live heap objects. This is NOT resident set size.",
	}, func() float64 { return float64(src.uint("/memory/classes/heap/objects:bytes")) })

	r.GaugeFunc(Opts{
		Name: "go_memory_total_bytes",
		Help: "Bytes mapped by the Go runtime for all purposes. This is NOT resident set size; see runtimecol.go.",
	}, func() float64 { return float64(src.uint("/memory/classes/total:bytes")) })

	// A FLOAT counter, and it has to be. /cpu/classes/gc/total:cpu-seconds is a
	// float64 that on a healthy process spends its whole life below one second,
	// so bridging it through a uint64 accessor — which is what this line did —
	// truncated every reading to 0 and published a flat zero under a help string
	// promising CPU seconds. See counterFloatFunc in counter.go: the TYPE line is
	// still `counter`, so rate() is unaffected.
	r.CounterFloatFunc(Opts{
		Name: "go_cpu_gc_seconds_total",
		Help: "CPU seconds spent in garbage collection, summed over all cores.",
	}, func() float64 { return src.float("/cpu/classes/gc/total:cpu-seconds") })

	// Re-bucketed onto our own boundaries rather than passed through. Go's own
	// /sched/latencies:seconds histogram has on the order of fifty to two
	// hundred buckets, and exporting them verbatim would add that many series to
	// this binary for a metric almost nobody queries at that resolution.
	// client_golang re-buckets it for the same reason.
	r.register(&family{
		name: "go_sched_latencies_seconds",
		help: "Time goroutines spent runnable before running, re-bucketed from Go's own histogram. _sum is an estimate; see runtimecol.go.",
		typ:  typeHistogram,
		c:    &schedLatencies{src: src},
	})
}

// runtimeSource reads the allowlist through one runtime/metrics.Read call and
// caches it briefly.
//
// The cache exists because Read can briefly stop the world, and a scrape calls
// six accessors: without it, one HTTP request would pay for six reads. The
// window is driven by the injected clock, so a test with a *clock.Fake gets
// exactly one read per advance and therefore a deterministic body.
type runtimeSource struct {
	reg *Registry

	mu      sync.Mutex
	samples []rtmetrics.Sample
	index   map[string]int
	last    time.Time
	read    bool
}

const runtimeCacheWindow = 100 * time.Millisecond

func newRuntimeSource(r *Registry) *runtimeSource {
	s := &runtimeSource{
		reg:     r,
		samples: make([]rtmetrics.Sample, len(runtimeNames)),
		index:   make(map[string]int, len(runtimeNames)),
	}
	for i, n := range runtimeNames {
		s.samples[i].Name = n
		s.index[n] = i
	}
	return s
}

func (s *runtimeSource) refresh() {
	if s.read && s.reg.clk.Since(s.last) < runtimeCacheWindow {
		return
	}
	rtmetrics.Read(s.samples)
	s.last, s.read = s.reg.clk.Now(), true
}

// uint reads a KindUint64 sample. A name the running toolchain does not know
// comes back as KindBad, and zero is the right answer for it: a Go version that
// renamed a metric should cost an empty panel, never a panic at scrape time on
// a server that is otherwise healthy.
func (s *runtimeSource) uint(name string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	v := s.samples[s.index[name]].Value
	if v.Kind() != rtmetrics.KindUint64 {
		return 0
	}
	return v.Uint64()
}

func (s *runtimeSource) float(name string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	v := s.samples[s.index[name]].Value
	if v.Kind() != rtmetrics.KindFloat64 {
		return 0
	}
	return v.Float64()
}

// withHistogram calls fold on the named histogram WITH s.mu STILL HELD, and
// that is the entire point of the shape.
//
// runtime/metrics documents that a pointer-typed value shares storage which a
// later Read REUSES, and that a caller who needs the data must deep-copy it.
// The previous spelling of this was `histogram(name) *Float64Histogram`: it took
// the lock, returned the runtime's own pointer, and released the lock, after
// which the renderer read h.Counts and h.Buckets with nothing held. Two
// concurrent scrapes are ordinary — Prometheus plus a human plus a
// readiness-adjacent curl — and the second one's refresh() calls
// rtmetrics.Read, which writes into exactly the arrays the first one is
// iterating. That is a data race the race detector sees and, worse, a torn
// histogram: bucket counts from two different instants, which can render a
// cumulative histogram whose _bucket series decrease across `le`. Prometheus
// does not reject such a document; histogram_quantile just returns nonsense.
//
// REJECTED: a snapshot buffer owned by runtimeSource, which is the obvious way
// to deep-copy without allocating. It only moves the race — two concurrent
// scrapes would then both be writing OUR shared scratch instead of reading the
// runtime's. The destination has to be per-scrape, so it is the CALLER's stack,
// and the fold runs here where the lock still is.
//
// fold is not called at all for a name this toolchain does not know, which
// leaves the caller's zero value intact: an empty histogram, for the same
// reason uint() answers 0. A renamed runtime metric costs a blank panel, never
// a panic at scrape time.
func (s *runtimeSource) withHistogram(name string, fold func(*rtmetrics.Float64Histogram)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	v := s.samples[s.index[name]].Value
	if v.Kind() != rtmetrics.KindFloat64Histogram {
		return
	}
	fold(v.Float64Histogram())
}

// schedLatencies renders Go's scheduling latency histogram on bucketsSched.
//
// It holds no mutable state, deliberately: one *schedLatencies is registered
// for the life of the process and every scrape renders through it
// concurrently, so a scratch field here would be shared across scrapes. The
// working state is schedFold, on the rendering goroutine's stack.
type schedLatencies struct{ src *runtimeSource }

func (c *schedLatencies) series() int { return len(bucketsSched) + 3 }

// schedBucketCap sizes the fixed array one scrape folds into.
//
// It is a CONSTANT rather than len(bucketsSched) because make([]uint64, n) with
// a non-constant n always heap-allocates, while an array of constant size that
// does not escape lives on the stack — and a per-scrape allocation is exactly
// what the note on Registry.WriteTo promises the render does not do per sample.
// RegisterRuntime asserts the constant still covers the slice, so growing
// bucketsSched past it is a boot failure rather than an out-of-range panic on
// the first scrape.
const schedBucketCap = 16

// schedFold is one scrape's working state: our own bucket counts, plus the
// running count and estimated sum. A value type, owned by the goroutine that
// declared it, which is what makes two concurrent scrapes independent of each
// other and of the runtime's shared storage.
type schedFold struct {
	counts [schedBucketCap]uint64
	total  uint64
	sum    float64
}

// absorb deep-copies Go's histogram into f by folding it, and is called with
// runtimeSource.mu held — see withHistogram for why that is not an accident.
//
// Folding Go's buckets into ours by their UPPER edge is the only mapping that
// keeps the result a valid cumulative histogram: a Go bucket spanning [lo, hi)
// contributes entirely to the first of our buckets whose bound is at or above
// hi, so no observation is ever counted below a boundary it might actually
// exceed.
func (f *schedFold) absorb(h *rtmetrics.Float64Histogram) {
	for i, n := range h.Counts {
		if n == 0 {
			continue
		}
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		if math.IsInf(lo, -1) || lo < 0 {
			lo = 0
		}
		f.sum += float64(n) * lo
		f.total += n

		// hi is +Inf for Go's last bucket, which falls through to j =
		// len(bucketsSched): our own overflow bucket, which is correct.
		j := len(bucketsSched)
		for k, b := range bucketsSched {
			if hi <= b {
				j = k
				break
			}
		}
		f.counts[j] += n
	}
}

// appendSamples renders the fold as a cumulative histogram.
//
// _sum is an ESTIMATE, and this is the honest cost of re-bucketing.
// runtime/metrics does not publish a total, so the sum is accumulated from each
// Go bucket's lower edge — which biases it low by at most the width of one Go
// bucket per observation. client_golang's Go collector has the same limitation
// for the same reason. The bucket counts and _count are exact; only _sum is
// approximate, which means quantiles are trustworthy and a raw average is not.
func (c *schedLatencies) appendSamples(dst []byte, name string, extra []labelPair) []byte {
	// fold is this scrape's private copy of the runtime's data. It is filled
	// under runtimeSource.mu and read here with nothing held, which is safe
	// precisely because nothing else can reach it.
	// Spelled as a func literal rather than the method value `fold.absorb`,
	// because withHistogram never stores its argument: escape analysis keeps
	// both the closure and fold itself on this stack, and a scrape stays as
	// allocation-light as it was before the deep copy.
	var fold schedFold
	c.src.withHistogram("/sched/latencies:seconds", func(h *rtmetrics.Float64Histogram) {
		fold.absorb(h)
	})

	pairs := make([]labelPair, len(extra), len(extra)+1)
	copy(pairs, extra)
	pairs = append(pairs, labelPair{name: "le"})

	var cum uint64
	for i, b := range bucketsSched {
		cum += fold.counts[i]
		pairs[len(pairs)-1].value = formatBound(b)
		dst = appendUintSample(dst, name+"_bucket", pairs, cum)
	}
	cum += fold.counts[len(bucketsSched)]
	pairs[len(pairs)-1].value = posInf
	dst = appendUintSample(dst, name+"_bucket", pairs, cum)

	dst = appendFloatSample(dst, name+"_sum", extra, fold.sum)
	dst = appendUintSample(dst, name+"_count", extra, fold.total)
	return dst
}
