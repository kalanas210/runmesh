package bench_test

import (
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/metrics"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// productionRegistry builds the metric set exactly as run() does: the real tool
// registry, the real route table, the real streaming-route list. Anything else
// would benchmark a vocabulary that does not ship, and the whole cost of the
// exposition render is a function of how many series the vocabulary implies.
func productionRegistry(b *testing.B) (*metrics.Registry, *metrics.Set) {
	b.Helper()
	r := metrics.NewRegistry(clock.System())
	registry := tools.Builtins(tools.Options{
		EnableTestTools: true,
		TaskImage:       "runmesh/task:dev",
		PythonImage:     "runmesh/python:dev",
	})
	s := metrics.NewSet(r, metrics.Vocabulary{
		Tools:           registry.Names(),
		Routes:          httpapi.RoutePatterns(),
		StreamingRoutes: httpapi.StreamingRoutePatterns(),
		Version:         "bench",
		GoVersion:       runtime.Version(),
	})
	return r, s
}

// BenchmarkMetricsRender measures one scrape: the whole exposition, written.
//
// This is the number that decides a scrape interval. A Prometheus server
// scraping every fifteen seconds asks this endpoint to serialise every series
// in the process, and the answer has to be comfortably smaller than the
// interval or the exporter becomes the load. It is also the number ADR 0012
// owes: hand-writing the text format instead of taking client_golang is only
// defensible if the hand-written one is not slower, and this is where that is
// checked rather than assumed.
//
// The two cases differ only in whether the labelled families have been touched,
// and the gap is larger than it looks. A vec is pre-materialised only when the
// cross product of its declared vocabularies is at or below
// metrics.preMaterialiseLimit (64); above it the family starts with no children
// and creates them on first observation. Three of the shipped families are over
// that line, including runmesh_step_attempts_total, so a cold registry renders
// their HELP and TYPE lines with no sample underneath. The warm number is the
// honest one to quote, because it is the only one that describes a server that
// has done any work.
func BenchmarkMetricsRender(b *testing.B) {
	b.Run("state=cold", func(b *testing.B) {
		r, _ := productionRegistry(b)
		benchRender(b, r)
	})
	b.Run("state=warm", func(b *testing.B) {
		r, s := productionRegistry(b)
		warm(s)
		benchRender(b, r)
	})
}

func benchRender(b *testing.B, r *metrics.Registry) {
	// One render before the timer, to size the buffer the writer grows and to
	// let the registry's own scrape histogram record its first observation.
	// Without it the first timed iteration pays an allocation every subsequent
	// one does not, which at these durations is visible.
	n, err := r.WriteTo(io.Discard)
	if err != nil {
		b.Fatalf("WriteTo: %v", err)
	}

	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := r.WriteTo(io.Discard); err != nil {
			b.Fatalf("WriteTo: %v", err)
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
	b.ReportMetric(float64(r.SeriesUpperBound()), "series")
	b.ReportMetric(float64(n), "bytes/scrape")
}

// BenchmarkObserver measures the engine's telemetry seam on the path it is
// actually called from.
//
// Every one of these methods runs on a dispatcher or a worker goroutine, and
// internal/engine/observer.go states the consequence in as many words: an
// implementation that takes a lock or allocates per call does not make metrics
// slow, it SHRINKS THE WORKER POOL, silently, showing up as reduced throughput
// with no error anywhere. TestObserveIsAllocationFree already pins the
// allocation half with testing.AllocsPerRun. This pins the other half — the
// absolute cost — so the claim that the seam is free can be stated in
// nanoseconds instead of in adjectives.
//
// AttemptSettled is the expensive one and is broken out for that reason: it is
// the only method that resolves three label vectors, and the only one whose
// label values are not all drawn from a closed vocabulary.
func BenchmarkObserver(b *testing.B) {
	outcome := engine.AttemptOutcome{
		Tool:     "echo",
		State:    runmesh.Succeeded,
		Stop:     engine.StopNone,
		Duration: 4 * time.Millisecond,
	}
	failed := engine.AttemptOutcome{
		Tool:     "python_execute",
		State:    runmesh.Failed,
		Stop:     engine.StopTimeout,
		Code:     runmesh.CodeTimeout,
		Duration: 30 * time.Second,
	}
	// A code no tool in the registry declares, so it resolves through the one
	// genuinely open label axis onto `other`. That lookup is the slowest path
	// through the vec and is the one worth knowing the cost of.
	unknown := engine.AttemptOutcome{
		Tool:     "not_a_registered_tool",
		State:    runmesh.Failed,
		Stop:     engine.StopNone,
		Code:     "some_tool_invented_this",
		Duration: time.Second,
	}

	for _, tc := range []struct {
		name string
		call func(s *metrics.Set)
	}{
		{"m=Claimed", func(s *metrics.Set) { s.Claimed(8, 8, 900*time.Microsecond, nil) }},
		{"m=Dispatched", func(s *metrics.Set) { s.Dispatched("echo", 120*time.Microsecond) }},
		{"m=AttemptStarted", func(s *metrics.Set) { s.AttemptStarted("echo", 200*time.Microsecond) }},
		{"m=ToolExecuted", func(s *metrics.Set) { s.ToolExecuted("echo", 3*time.Millisecond) }},
		{"m=AttemptSettled/ok", func(s *metrics.Set) { s.AttemptSettled(outcome) }},
		{"m=AttemptSettled/failed", func(s *metrics.Set) { s.AttemptSettled(failed) }},
		{"m=AttemptSettled/other", func(s *metrics.Set) { s.AttemptSettled(unknown) }},
		{"m=Heartbeat", func(s *metrics.Set) { s.Heartbeat("echo", "ok") }},
		{"m=StoreOperation", func(s *metrics.Set) { s.StoreOperation("claim", 800*time.Microsecond, "") }},
		{"m=RequestFinished", func(s *metrics.Set) {
			s.RequestFinished("POST /api/v1/jobs", http.StatusCreated, 512, 2*time.Millisecond)
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			_, s := productionRegistry(b)
			call := tc.call
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				call(s)
			}
			b.StopTimer()
			reportRate(b, b.N)
		})
	}
}

// BenchmarkObserverParallel is the same seam with every core hitting it at
// once, which is the only configuration that can show contention.
//
// A serial benchmark of an atomic increment proves nothing about a sixteen-
// worker pool: a mutex looks free until two goroutines want it. The shipped
// implementation is atomics plus one map read under a read lock, so the
// expectation is that this number is close to the serial one — and "close" is
// a claim that needs a number next to it.
func BenchmarkObserverParallel(b *testing.B) {
	outcome := engine.AttemptOutcome{
		Tool:     "echo",
		State:    runmesh.Succeeded,
		Stop:     engine.StopNone,
		Duration: 4 * time.Millisecond,
	}
	_, s := productionRegistry(b)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.AttemptSettled(outcome)
		}
	})
	b.StopTimer()
	reportRate(b, b.N)
}

// warm touches every labelled family once, so a render measures the exposition
// a running server produces rather than the one a server produces in its first
// second.
func warm(s *metrics.Set) {
	s.Claimed(8, 8, time.Millisecond, nil)
	s.Dispatched("echo", time.Millisecond)
	s.DispatcherIdle("hint")
	s.AttemptStarted("echo", time.Millisecond)
	s.ToolExecuted("echo", time.Millisecond)
	s.AttemptSettled(engine.AttemptOutcome{
		Tool: "echo", State: runmesh.Succeeded, Stop: engine.StopNone, Duration: time.Millisecond,
	})
	s.Heartbeat("echo", "ok")
	s.LeaseReclaimed(runmesh.Queued)
	s.SweepFinished(1, time.Millisecond, nil)
	s.StoreOperation("claim", time.Millisecond, "")
	s.RequestFinished("POST /api/v1/jobs", http.StatusCreated, 512, time.Millisecond)
}
