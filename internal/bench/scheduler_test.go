package bench_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/storetest"
)

// BenchmarkScheduler is the number the README wants: steps per second through
// the whole runtime, with the tool subtracted out.
//
// WHAT IS INSIDE THE MEASUREMENT. One Store.Claim per batch, the unbuffered
// hand-off to a worker, Store.Start, one Executor.Execute that returns a
// constant, engine.Classify, Store.Finish, the event append and the job rollup.
// That is every line of the runtime a step touches. What is deliberately NOT
// inside it is the tool: nullExecutor does nothing, so a change in this number
// is a change in the scheduler and cannot be a change in what the scheduler was
// scheduling. Measuring with tools.Local and the echo tool would produce a
// larger, friendlier number that moves for reasons unrelated to the engine.
//
// WHY THE WORKER-POOL AXIS. RUNMESH_WORKERS is the first knob an operator
// reaches for, and the honest answer to "should I raise it" is a curve, not a
// number. With an in-process tool that never blocks, a worker is pure overhead
// past the point where the store can feed it — so the shape here says where the
// store becomes the ceiling, and it says it differently for the two stores,
// which is the entire reason the pgstore row exists.
//
// WHY THE CLOCK IS REAL. Every other runtime test in this repository drives a
// clock.Fake, because determinism is worth more than realism when the thing
// under test is a state machine. A throughput number is the one case where that
// inverts: a fake clock measures how fast the test can advance a counter. So
// this is the system clock, real goroutines and real scheduling — and the rate
// still comes from testing.B.Elapsed() rather than from a subtraction of two
// time.Now calls, because internal/clock/purity_test.go walks this file too and
// the exemption is not needed.
func BenchmarkScheduler(b *testing.B) {
	for _, be := range backends(b) {
		for _, workers := range []int{1, 2, 4, 8, 16} {
			b.Run("store="+be.name+"/workers="+strconv.Itoa(workers), func(b *testing.B) {
				benchScheduler(b, be, workers)
			})
		}
	}
}

func benchScheduler(b *testing.B, be backend, workers int) {
	s := be.open(b)
	clk := clock.System()
	// Rounded up to whole jobs, then measured against what fill actually
	// created. The engine drains the queue completely, so the unit count has to
	// be the real one or the rate is wrong by up to one job.
	//
	// Stamped with the SYSTEM clock rather than the package epoch. The engine
	// reads its own clock to evaluate readiness, so a queue stamped with a
	// fabricated constant is claimable or not depending on which side of that
	// constant the machine's wall clock is on — see fill's own comment for what
	// that failure looks like from the outside.
	total := fill(b, s, b.N, clk.Now())
	if total == 0 {
		b.Skip("b.N rounded to zero steps")
	}

	// A generous subscription buffer and a consumer that does nothing but
	// count. The fan-out is a non-blocking send that DROPS rather than blocks —
	// which is correct for the store and fatal for a benchmark that uses the
	// stream as its completion signal, so the drop counter is checked at the
	// end rather than assumed to be zero.
	events, unsub := s.Subscribe(1 << 16)
	defer unsub()

	done := make(chan struct{})
	go func() {
		var finished int
		for e := range events {
			if e.Type != runmesh.StepFinished {
				continue
			}
			finished++
			if finished == total {
				close(done)
				return
			}
		}
	}()

	eng := newBenchEngine(b, s, workers)

	b.ReportAllocs()
	b.ResetTimer()
	if err := eng.Start(context.Background()); err != nil {
		b.Fatalf("engine.Start: %v", err)
	}
	select {
	case <-done:
	case <-clk.After(2 * time.Minute):
		b.Fatalf("only some of %d steps finished within two minutes; inflight=%d",
			total, eng.Inflight())
	}
	b.StopTimer()

	shutdownEngine(b, eng)

	if n := s.Dropped(); n != 0 {
		b.Fatalf("the event fan-out dropped %d events: the completion signal this "+
			"benchmark times against is lossy, so the rate cannot be trusted", n)
	}
	if e := b.Elapsed(); e > 0 {
		b.ReportMetric(float64(total)/e.Seconds(), "steps/sec")
	}
	// ns/step rather than ns/op: b.N and the step count differ by the rounding
	// to whole jobs, and a reader comparing this column against the store-op
	// table needs the two to be the same unit.
	if total > 0 {
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(total), "ns/step")
	}
}

// newBenchEngine builds the engine every throughput case runs.
//
// The timings are compressed deliberately and the reasons are not the same for
// each. PollInterval is short because the dispatcher's ticker is a BACKSTOP
// behind Store.Ready(), and a 250ms default would put quarter-second stalls
// into a measurement of microseconds whenever a hint was lost. HeartbeatInterval
// is long because a step that finishes in microseconds never heartbeats in
// production either, and a heartbeat firing inside this loop would be measuring
// a write that a real step of this length would not make. ReconcileInterval is
// an hour because nothing here ever loses a lease, and a sweep landing in the
// middle of a run would show up as a step that inexplicably took longer.
func newBenchEngine(b *testing.B, s storetest.Store, workers int) *engine.Engine {
	b.Helper()

	eng, err := engine.New(engine.Config{
		Owner:             "bench",
		Workers:           workers,
		ClaimBatch:        workers,
		PollInterval:      time.Millisecond,
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		StoreTimeout:      10 * time.Second,
		AbandonGrace:      5 * time.Second,
		ReconcileInterval: time.Hour,
		ReconcileBatch:    100,
		Backoff:           engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2},
	}, engine.Deps{
		Store:    s,
		Executor: nullExecutor{},
		Clock:    clock.System(),
		Log:      quiet(),
	})
	if err != nil {
		b.Fatalf("engine.New: %v", err)
	}
	return eng
}

// shutdownEngine drains the pool after the timer has stopped. The deadline is
// real time and it is real: a drain that does not finish means a worker is
// still holding a step, and a benchmark that left one running would corrupt the
// next sub-case's store rather than fail its own.
func shutdownEngine(b *testing.B, eng *engine.Engine) {
	b.Helper()
	ctx, cancel := clock.WithWriteDeadline(context.Background(), 30*time.Second)
	defer cancel()
	if err := eng.Shutdown(ctx); err != nil {
		b.Fatalf("engine.Shutdown: %v", err)
	}
}
