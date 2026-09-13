package metrics

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
)

// Runtime is the slice of the engine this package needs, declared HERE by its
// consumer for the same reason engine.Store and httpapi.Runtime are: so that
// neither package has to import the other, and so a test can satisfy it in
// four lines.
//
// It deliberately does NOT carry ClaimErrors or LeasesExpired, even though
// engine.Stats() has both. Those two numbers are already counted, WITH labels,
// by runmesh_claims_total{outcome="error"} and runmesh_leases_expired_total,
// and exporting them a second time as a derived gauge is exactly the drift the
// design notes warn about: two paths to one number, agreeing until somebody
// adds an early return on one of them. The engine's own Stats() stays as it is,
// feeding GET /api/v1/ready, and TestClaimErrorsMetricMatchesStats and
// TestLeasesExpiredMetricMatchesStats assert that the counter and the snapshot
// still agree after a real harness run.
type Runtime interface {
	Workers() int
	Inflight() int
	IdleWorkers() int
}

// StoreSource is the slice of the store this package needs.
type StoreSource interface {
	QueueDepth(ctx context.Context, now time.Time) (int, error)
	Dropped() uint64
}

// Streams is the slice of the HTTP API this package needs: how many live event
// streams are open right now.
//
// It is a GAUGE pulled from the API rather than anything derived from the
// request metrics, and the reason is the one trap Week 6's two features set for
// each other. runmesh_http_request_duration_seconds is observed from Logger's
// deferred func, which runs when a handler RETURNS — and an SSE handler returns
// when somebody closes a browser tab, minutes or hours later. Left alone, a
// single dashboard would drop an hour-long observation into the same histogram
// as a four-millisecond readiness probe and every latency panel in the
// deployment would silently be describing tab lifetimes. So the streaming
// routes are excluded from that histogram (RequestFinished consults
// streamingRoutes) and their COUNT is exported here instead, which is the
// quantity an operator actually wanted.
type Streams interface {
	ActiveStreams() int
}

// holder is an atomically swappable optional value. Bind* is called after the
// listener could in principle already be serving, so a plain field write would
// be a data race against a scrape; a mutex would work too, but this costs one
// atomic load per gauge per scrape and no contention at all.
type holder[T any] struct{ p atomic.Pointer[boxed[T]] }

type boxed[T any] struct{ v T }

func (h *holder[T]) set(v T) { h.p.Store(&boxed[T]{v: v}) }

func (h *holder[T]) get() (T, bool) {
	if b := h.p.Load(); b != nil {
		return b.v, true
	}
	var zero T
	return zero, false
}

// BindRuntime attaches the engine. Until it is called the runtime gauges read
// zero rather than being absent, which is deliberate: a series that appears
// halfway through a process's life looks, on a graph, exactly like a process
// that restarted.
func (s *Set) BindRuntime(r Runtime) { s.runtimeSrc.set(r) }

// BindStore attaches the store.
func (s *Set) BindStore(st StoreSource) { s.storeSrc.set(st) }

// BindStreams attaches the HTTP API, whose admission counter already holds the
// number of open event streams. Binding it rather than counting opens and
// closes here means there is one counter, not two that agree until somebody
// adds an early return on one path.
func (s *Set) BindStreams(st Streams) { s.streamSrc.set(st) }

// registerGauges declares every scrape-time reading.
//
// Every one of them is a GaugeFunc or a CounterFunc rather than a value this
// package keeps up to date, and that is the whole design: NOTHING IS MIRRORED,
// so nothing can drift from engine.Stats() or from the store's own counters.
// It also means the registry owns no goroutine — which matters more than it
// looks, because the engine's package doc fixes the goroutine census at
// 2 + Workers + one per in-flight step, and an aggregation loop started
// anywhere in this process would contradict a stated design invariant while
// nothing failed.
func (s *Set) registerGauges(r *Registry) {
	r.GaugeFunc(Opts{
		Name: "runmesh_workers",
		Help: "Configured size of the worker pool.",
	}, func() float64 { return float64(s.runtimeInt(func(rt Runtime) int { return rt.Workers() })) })

	r.GaugeFunc(Opts{
		Name: "runmesh_workers_inflight",
		Help: "Steps executing right now.",
	}, func() float64 { return float64(s.runtimeInt(func(rt Runtime) int { return rt.Inflight() })) })

	// Idle is NOT workers minus inflight, and the difference is the interesting
	// window: a capacity token is taken by the dispatcher when it decides to
	// claim and is not consumed until a worker takes the lease off the channel,
	// so during the claim-to-hand-off gap the pool has fewer idle tokens than
	// it has spare workers. Exporting both is what makes a store that is slow to
	// claim distinguishable from a pool that is genuinely busy.
	r.GaugeFunc(Opts{
		Name: "runmesh_workers_idle",
		Help: "Unclaimed capacity tokens: free workers the dispatcher has not yet spent on a claim.",
	}, func() float64 { return float64(s.runtimeInt(func(rt Runtime) int { return rt.IdleWorkers() })) })

	r.GaugeFunc(Opts{
		Name: "runmesh_queue_depth",
		Help: "Steps that are claimable right now. Cached; see the note on the minimum recomputation interval in gauges.go.",
	}, func() float64 { return float64(s.depth.value()) })

	// The companion to the request-duration histogram's deliberate blind spot.
	// A stream is not a request that takes a long time, it is a connection that
	// is held, and those are different quantities that want different
	// instruments — so the duration histogram declines to measure it and this
	// gauge reports it honestly.
	r.GaugeFunc(Opts{
		Name: "runmesh_http_streams_active",
		Help: "Event-stream connections open right now. Streaming routes are excluded from the request-duration histogram, which measures completed requests; this is the quantity that replaces them there.",
	}, func() float64 {
		if st, ok := s.streamSrc.get(); ok {
			return float64(st.ActiveStreams())
		}
		return 0
	})

	r.CounterFunc(Opts{
		Name: "runmesh_store_events_dropped_total",
		Help: "Events the store could not deliver to an in-process subscriber because that subscriber was too slow.",
	}, func() uint64 {
		if st, ok := s.storeSrc.get(); ok {
			return st.Dropped()
		}
		return 0
	})
}

func (s *Set) runtimeInt(read func(Runtime) int) int {
	if rt, ok := s.runtimeSrc.get(); ok {
		return read(rt)
	}
	return 0
}

// depthMinInterval is how often runmesh_queue_depth may actually be recomputed.
//
// THIS CACHE IS NOT AN OPTIMISATION, IT IS A SAFETY INTERLOCK, and the reason
// is worth writing out. memstore.QueueDepth walks every job times every step
// calling jobstate.Claimable, under the single global mutex that also
// serialises Claim, Finish, Heartbeat and ExpireLeases — so an uncached scrape
// adds a stop-the-world pause to every claim in the process. pgstore.QueueDepth
// is a count(*) over the full claimable predicate with two correlated NOT
// EXISTS subqueries, which is a sequential scan per scrape. Either one is fine
// occasionally and ruinous at a one-second scrape interval.
//
// Five seconds makes it safe at the usual fifteen-second interval and makes a
// one-second interval pointless rather than dangerous, which is the right shape
// for a knob nobody should have to know about. Note also that
// GET /api/v1/ready already pays the UNCACHED cost on every probe: a tight
// readiness probe plus a tight scrape is the combination that hurts, and this
// cache removes one half of it.
const depthMinInterval = 5 * time.Second

// depthCache is a single-flight cache over StoreSource.QueueDepth.
//
// The mutex is held across the store call deliberately: it is what makes this
// single-flight, so two concurrent scrapes cannot both start a table scan. That
// is safe here in a way it would not be on an engine goroutine — a scrape has
// nobody waiting on it and holds no pool capacity.
type depthCache struct {
	clk clock.Clock
	src *holder[StoreSource]

	mu      sync.Mutex
	value_  int
	last    time.Time
	sampled bool
}

func newDepthCache(clk clock.Clock, src *holder[StoreSource]) *depthCache {
	return &depthCache{clk: clk, src: src}
}

func (d *depthCache) value() int {
	st, ok := d.src.get()
	if !ok {
		return 0
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sampled && d.clk.Since(d.last) < depthMinInterval {
		return d.value_
	}

	// A detached, bounded context: the scrape's own request context is not
	// available here (a GaugeFunc takes none, on purpose — a collector that can
	// be cancelled halfway renders a partial document), and an unbounded store
	// call would let one wedged query hold this mutex for as long as the store
	// is unhealthy.
	ctx, cancel := clock.WithWriteDeadline(context.Background(), depthMinInterval)
	n, err := st.QueueDepth(ctx, d.clk.Now())
	cancel()
	if err != nil {
		// Keep serving the last good value rather than reporting zero. A store
		// outage already fails readiness loudly; a queue depth that drops to
		// zero at the same moment would read on a dashboard as "the queue
		// drained", which is the opposite of what happened.
		d.last = d.clk.Now()
		return d.value_
	}
	d.value_, d.last, d.sampled = n, d.clk.Now(), true
	return n
}
