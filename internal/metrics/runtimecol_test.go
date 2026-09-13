package metrics

import (
	"io"
	"reflect"
	"runtime"
	rtmetrics "runtime/metrics"
	"strings"
	"sync"
	"testing"

	"github.com/kalanas210/runmesh/internal/clock"
)

// runtimeRegistry is a registry carrying ONLY the Go runtime bridge, on the
// system clock.
//
// The system clock rather than a clock.Fake, deliberately: runtimeSource caches
// its Read behind clk.Since, and a fake clock that never advances would let
// exactly one rtmetrics.Read happen for the life of the test — which is the
// opposite of what the concurrency test below needs, since the race being
// hunted is a Read landing in the middle of a fold.
func runtimeRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(clock.System())
	RegisterRuntime(r)
	return r
}

// TestGCSecondsCounterKeepsItsFraction is the direct regression for a counter
// that read zero for ever.
//
// go_cpu_gc_seconds_total is sourced from /cpu/classes/gc/total:cpu-seconds,
// which is a float64 and which on a healthy process spends its whole life well
// below one second. Bridged through CounterFunc (func() uint64) the conversion
// floored it, so the rendered value was 0 on every scrape of every normal
// process — while the help string promised CPU seconds and ADR 0012 claimed the
// value was recovered exactly.
func TestGCSecondsCounterKeepsItsFraction(t *testing.T) {
	// Not t.Parallel: it forces collections, and the whole point is to read a
	// runtime counter it has just moved.
	//
	// It collects until the runtime reports a NONZERO reading rather than a
	// fixed number of times, because the underlying clock is coarse — on
	// Windows the timer granularity is around 15 ms, so a handful of GCs of an
	// idle heap genuinely accumulate zero CPU-seconds and the test would be
	// asserting on a number the platform never produced. The loop is bounded,
	// and it synchronises on the reading itself; there is no sleep.
	const attempts = 400
	for range attempts {
		if gcCPUSeconds() > 0 {
			break
		}
		runtime.GC()
	}

	r := runtimeRegistry(t)
	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["go_cpu_gc_seconds_total"]
	if f == nil {
		t.Fatal("go_cpu_gc_seconds_total is not registered")
	}
	if f.typ != typeCounter {
		t.Errorf("TYPE is %q, want counter: a cumulative duration is still a "+
			"counter, and rate() over it is still the right operator", f.typ)
	}
	if len(f.samples) != 1 {
		t.Fatalf("%d samples, want 1", len(f.samples))
	}

	// What the runtime itself says, read the same way the collector reads it.
	// If the toolchain reports a true zero there is nothing for this test to
	// distinguish, and skipping is more honest than asserting on a number the
	// platform did not produce.
	source := gcCPUSeconds()
	if source < 0 {
		t.Skip("this toolchain does not publish /cpu/classes/gc/total:cpu-seconds as a float64")
	}
	if source == 0 {
		t.Skip("the runtime reports exactly zero GC CPU seconds; there is no fraction to lose")
	}
	if source >= 1 {
		t.Skipf("this process has already spent %v CPU-seconds in GC, so truncation "+
			"would not be visible; the test cannot distinguish the two bridges", source)
	}

	// The fraction survived. Through the uint64 bridge this was 0.
	if got := f.samples[0].value; got == 0 {
		t.Errorf("go_cpu_gc_seconds_total rendered 0 against a runtime reading of "+
			"%v; the value is being floored by an integer bridge", source)
	}
}

// gcCPUSeconds reads the runtime's own number directly, bypassing this
// package, so the assertion above compares the exposition against the source
// rather than against itself. It answers -1 for a toolchain that does not
// publish the metric as a float64.
func gcCPUSeconds() float64 {
	probe := []rtmetrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}}
	rtmetrics.Read(probe)
	if probe[0].Value.Kind() != rtmetrics.KindFloat64 {
		return -1
	}
	return probe[0].Value.Float64()
}

// TestFloatCounterRendersItsFraction pins the mechanism on its own, without the
// runtime in the way: a counter registered through CounterFloatFunc renders the
// value it was given and still declares itself a counter.
func TestFloatCounterRendersItsFraction(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	r.CounterFloatFunc(Opts{
		Name: "test_cpu_seconds_total",
		Help: "A cumulative duration that is nowhere near a whole second.",
	}, func() float64 { return 0.0625 })

	body := render(t, r)
	if !strings.Contains(body, "# TYPE test_cpu_seconds_total counter\n") {
		t.Errorf("a float-valued counter did not declare itself a counter:\n%s", body)
	}
	if !strings.Contains(body, "test_cpu_seconds_total 0.0625\n") {
		t.Errorf("the fraction did not survive the render:\n%s", body)
	}
}

// TestRuntimeHistogramIsFoldedUnderTheLock is the DETERMINISTIC half of the
// A3 regression, and it is the half that matters.
//
// The race detector is the obvious test for a pointer that escaped its mutex,
// and TestConcurrentScrapesOfTheRuntimeCollector below is that test — but a
// hammer test only fails when the interleaving happens to occur, and -race
// needs a working cgo toolchain that not every developer machine has. So the
// invariant is asserted head-on instead: whatever reads the runtime's shared
// storage must do it with runtimeSource.mu HELD, because that lock is the only
// thing standing between a fold and another goroutine's rtmetrics.Read writing
// into the same arrays.
//
// TryLock from inside the callback is the whole trick: sync.Mutex is not
// reentrant, so a successful TryLock proves the lock is NOT held, which is
// exactly the defect — withHistogram's predecessor took the lock, returned the
// runtime's pointer, released the lock, and let the renderer iterate
// unprotected.
func TestRuntimeHistogramIsFoldedUnderTheLock(t *testing.T) {
	t.Parallel()

	src := newRuntimeSource(NewRegistry(clock.System()))

	folded := false
	src.withHistogram("/sched/latencies:seconds", func(h *rtmetrics.Float64Histogram) {
		folded = true
		if h == nil {
			t.Error("fold was called with a nil histogram")
		}
		if src.mu.TryLock() {
			src.mu.Unlock()
			t.Error("the fold runs with runtimeSource.mu RELEASED: another scrape's " +
				"rtmetrics.Read can rewrite h.Counts and h.Buckets while this one " +
				"is iterating them, which runtime/metrics documents as the reason " +
				"a pointer-typed value must be deep-copied before the lock drops")
		}
	})
	if !folded {
		t.Fatal("/sched/latencies:seconds did not reach the fold at all; " +
			"this toolchain does not publish it as a Float64Histogram and the " +
			"assertion above was vacuous")
	}
}

// TestConcurrentScrapesOfTheRuntimeCollector is the -race regression for a
// pointer that escaped its mutex.
//
// runtimeSource.histogram() used to take s.mu, return the runtime's own
// *runtime/metrics.Float64Histogram, and release the lock; the sched-latency
// renderer then walked h.Counts and h.Buckets with nothing held.
// runtime/metrics documents that pointer-typed values share storage which a
// later Read REUSES, so a second scrape's refresh() wrote into exactly the
// arrays the first scrape was iterating. Two concurrent scrapes are entirely
// ordinary — a Prometheus server and one curl are enough.
//
// The goroutines here also call the scalar accessors, because those are what
// trigger refresh(): the race needs a Read to land inside a fold, and hammering
// only the histogram would mostly hit the cached copy.
func TestConcurrentScrapesOfTheRuntimeCollector(t *testing.T) {
	t.Parallel()

	r := runtimeRegistry(t)

	const scrapers = 8
	const each = 60

	var wg sync.WaitGroup
	for range scrapers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				// Churn, so the runtime has fresh numbers to write over the
				// ones another goroutine is reading.
				runtime.Gosched()
				if _, err := r.WriteTo(io.Discard); err != nil {
					t.Errorf("WriteTo: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// And the document is still a valid cumulative histogram afterwards. A torn
	// read is not only a race: it mixes bucket counts from two instants, which
	// can produce _bucket series that DECREASE across `le` — a document
	// Prometheus accepts and histogram_quantile then answers nonsense from.
	families, err := parseExposition(render(t, r))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := families["go_sched_latencies_seconds"]
	if f == nil {
		t.Fatal("go_sched_latencies_seconds is missing")
	}
	var prev float64
	buckets := 0
	for _, s := range f.samples {
		if !strings.HasSuffix(s.name, "_bucket") {
			continue
		}
		buckets++
		if s.value < prev {
			t.Errorf("the cumulative histogram decreases at le=%q: %v after %v",
				s.labels["le"], s.value, prev)
		}
		prev = s.value
	}
	if buckets != len(bucketsSched)+1 {
		t.Errorf("%d bucket series, want %d", buckets, len(bucketsSched)+1)
	}
}

// TestSchedFoldIsOwnedByTheScrape asserts the property that makes the fix a fix
// rather than a narrower window: the destination of the deep copy is per-call,
// so two scrapes cannot share it.
//
// It is checkable because schedLatencies holds no mutable state at all — one
// instance is registered for the life of the process and every scrape renders
// through it — so a scratch buffer hung off the collector, or off
// runtimeSource, would have moved the race from the runtime's arrays to ours
// rather than removing it. A struct with a single pointer field is the shape
// that says so.
func TestSchedFoldIsOwnedByTheScrape(t *testing.T) {
	t.Parallel()

	if got := reflect.TypeOf(schedLatencies{}).NumField(); got != 1 {
		t.Errorf("schedLatencies has %d fields, want 1 (just the source); "+
			"per-scrape working state belongs on the rendering goroutine's stack, "+
			"because one collector serves every concurrent scrape", got)
	}

	// The fold itself is a value type, so declaring one costs nothing shared.
	var a, b schedFold
	h := &rtmetrics.Float64Histogram{
		Counts:  []uint64{1, 2},
		Buckets: []float64{0, 1e-5, 1e-4},
	}
	a.absorb(h)
	if b.total != 0 {
		t.Error("two folds share state")
	}
	if a.total != 3 {
		t.Errorf("total = %d, want 3", a.total)
	}
}
