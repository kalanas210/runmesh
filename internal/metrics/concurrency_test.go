package metrics

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// sampleOutcome is a settled attempt of the shape the engine actually produces:
// every label value drawn from the closed vocabulary, so the hot path is the
// pre-materialised one.
var sampleOutcome = engine.AttemptOutcome{
	Tool:     "echo",
	State:    runmesh.Failed,
	Stop:     engine.StopTimeout,
	Code:     runmesh.CodeTimeout,
	Duration: 1234567,
}

// TestObserversAndRendererAreRaceFree runs the whole observer surface from many
// goroutines while another renders, which is the shape production actually has:
// N workers and a dispatcher writing while a scrape reads.
//
// It is here for -race, and the assertion is that the counts add up afterwards
// — a lock-free counter that loses increments under contention would pass a
// race detector and still be wrong.
func TestObserversAndRendererAreRaceFree(t *testing.T) {
	t.Parallel()

	r, s := productionSet(t)

	const writers = 8
	const perWriter = 500

	var writersWG, rendererWG sync.WaitGroup
	stop := make(chan struct{})

	// The renderer, running throughout. It gets its own WaitGroup: it stops
	// only when the writers are done, so waiting on one group for both would
	// deadlock.
	rendererWG.Add(1)
	go func() {
		defer rendererWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := r.WriteTo(io.Discard); err != nil {
				t.Errorf("WriteTo: %v", err)
				return
			}
		}
	}()

	for range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for range perWriter {
				s.AttemptSettled(sampleOutcome)
				s.AttemptStarted("echo", 1000)
				s.ToolExecuted("echo", 2000)
				s.Heartbeat("echo", "ok")
				s.Claimed(4, 2, 3000, nil)
				s.Dispatched("echo", 4000)
				s.DispatcherIdle("hint")
				s.LeaseReclaimed(runmesh.Queued)
				s.SweepFinished(1, 5000, nil)
				s.ConcurrencyAdjusted(4, 5)
				s.StoreOperation("claim", 6000, "")
				s.RequestFinished("GET /api/v1/jobs", 200, 128, 7000)
			}
		}()
	}
	writersWG.Wait()
	close(stop)
	rendererWG.Wait()

	want := uint64(writers * perWriter)
	if got := s.attempts.With("echo", "FAILED", "timeout").Value(); got != want {
		t.Errorf("attempts = %d, want %d: increments were lost under contention", got, want)
	}
	if got := s.attemptFailures.With("echo", runmesh.CodeTimeout).Value(); got != want {
		t.Errorf("failures = %d, want %d", got, want)
	}
	if got := s.heartbeats.With("echo", "ok").Value(); got != want {
		t.Errorf("heartbeats = %d, want %d", got, want)
	}
	if got := s.attemptDuration.With("echo").Count(); got != want {
		t.Errorf("attempt duration observations = %d, want %d", got, want)
	}
	if got := s.claimReq.Value(); got != 4*want {
		t.Errorf("leases requested = %d, want %d", got, 4*want)
	}
	if got := s.concurrencyAdjustments.With("grow").Value(); got != want {
		t.Errorf("concurrency adjustments = %d, want %d: increments were lost under contention", got, want)
	}

	// And the body still parses after all of that, which is the property a
	// half-written render would break.
	var buf bytes.Buffer
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if _, err := parseExposition(buf.String()); err != nil {
		t.Fatalf("the body no longer parses: %v", err)
	}
}

// TestObserveIsAllocationFree is the checkable form of the contract written out
// on engine.Observer: every method runs on a goroutine that is holding a worker
// pool capacity token, so an implementation that allocates per call does not
// make metrics slow — it shrinks the worker pool, silently, showing up as
// reduced throughput with no error anywhere.
//
// The type system cannot enforce that. This can. It is deliberately NOT
// t.Parallel: AllocsPerRun pins GOMAXPROCS for the duration.
func TestObserveIsAllocationFree(t *testing.T) {
	_, s := productionSet(t)
	h := s.attemptDuration.With("echo")

	tests := []struct {
		name string
		fn   func()
	}{
		{"Histogram.Observe", func() { h.Observe(0.5) }},
		{"Histogram.ObserveDuration", func() { h.ObserveDuration(1234567) }},
		{"AttemptSettled", func() { s.AttemptSettled(sampleOutcome) }},
		{"AttemptStarted", func() { s.AttemptStarted("echo", 1000) }},
		{"ToolExecuted", func() { s.ToolExecuted("echo", 1000) }},
		{"Heartbeat", func() { s.Heartbeat("echo", "ok") }},
		{"Claimed", func() { s.Claimed(4, 2, 1000, nil) }},
		{"Dispatched", func() { s.Dispatched("echo", 1000) }},
		{"DispatcherIdle", func() { s.DispatcherIdle("tick") }},
		{"LeaseReclaimed", func() { s.LeaseReclaimed(runmesh.Queued) }},
		{"SweepFinished", func() { s.SweepFinished(1, 1000, nil) }},
		{"ConcurrencyAdjusted", func() { s.ConcurrencyAdjusted(4, 5) }},
		{"StoreOperation", func() { s.StoreOperation("claim", 1000, "") }},
		{"RequestFinished", func() { s.RequestFinished("GET /api/v1/jobs", 200, 64, 1000) }},
	}
	for _, tc := range tests {
		// Warm the lazily created children first, so the one allocation a miss
		// legitimately makes is not counted against the steady state this is
		// measuring.
		tc.fn()
		if n := testing.AllocsPerRun(200, tc.fn); n != 0 {
			t.Errorf("%s allocates %.1f times per call, want 0; "+
				"an allocation here is an allocation per settled attempt, on a "+
				"goroutine holding pool capacity", tc.name, n)
		}
	}
}

// TestLabelLookupIsAllocationFreeEvenWhenRejected: the out-of-vocabulary path
// must not be a way to make the hot path allocate. A tool emitting its own
// error code takes it on EVERY attempt it fails, so it is not an edge case.
func TestLabelLookupIsAllocationFreeEvenWhenRejected(t *testing.T) {
	_, s := productionSet(t)
	rejected := sampleOutcome
	rejected.Code = "some_tool_specific_code"

	s.AttemptSettled(rejected) // warm the "other" child
	if n := testing.AllocsPerRun(200, func() { s.AttemptSettled(rejected) }); n != 0 {
		t.Errorf("a rejected label value allocates %.1f times per call, want 0", n)
	}
}
