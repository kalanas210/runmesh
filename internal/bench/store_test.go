package bench_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// BenchmarkStoreOp measures the four writes a step costs, one at a time.
//
// They are separated because they are not interchangeable. Claim is a queue
// scan under contention; Start is a single guarded row update; Heartbeat is the
// hottest write in the system and is the one the pgstore doc singles out as
// extending a lease rather than taking a lock; Finish is the whole terminal
// transition, the event append and the job rollup inside one critical section.
// A single "steps per second" number hides which of those four a change moved,
// and the first question after a throughput regression is exactly that.
//
// Every sub-case reports ops/sec alongside ns/op. They are the same number
// inverted, and having both written down is the difference between a table a
// reader can compare against a queue depth and one they have to do arithmetic
// on.
func BenchmarkStoreOp(b *testing.B) {
	for _, be := range backends(b) {
		b.Run("store="+be.name, func(b *testing.B) {
			b.Run("op=Ping", func(b *testing.B) { benchPing(b, be) })
			b.Run("op=Claim", func(b *testing.B) { benchClaim(b, be) })
			b.Run("op=Start", func(b *testing.B) { benchStart(b, be) })
			b.Run("op=Heartbeat", func(b *testing.B) { benchHeartbeat(b, be) })
			b.Run("op=Finish", func(b *testing.B) { benchFinish(b, be) })
		})
	}
}

// benchPing measures a round trip that does NO work, and it is the row that
// makes every other row in this table readable.
//
// Store.Ping is a mutex check on memstore and a `SELECT 1` on pgstore, so its
// cost is the floor: connection acquisition, the wire, the server's parse of a
// trivial statement, and the wire back. Every pgstore number below is that
// floor plus the work, and without it a reader has no way to tell a slow query
// from a slow network — which on a developer machine talking to a container
// through a virtualised loopback is very often the whole story, and is exactly
// the confusion this row exists to remove.
func benchPing(b *testing.B, be backend) {
	s := be.open(b)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := s.Ping(ctx); err != nil {
			b.Fatalf("Ping: %v", err)
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

func benchClaim(b *testing.B, be backend) {
	s := be.open(b)
	// One spare batch beyond b.N: Claim returning fewer than Limit is normal
	// and is not an error, so a queue sized exactly to b.N would let the last
	// iteration measure an empty scan instead of a claim.
	fill(b, s, b.N+stepsPerJob, epoch)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		leases, err := s.Claim(ctx, runmesh.ClaimRequest{
			Owner: "bench", Limit: 1, LeaseTTL: benchLeaseTTL, Now: epoch,
		})
		if err != nil {
			b.Fatalf("Claim: %v", err)
		}
		if len(leases) == 0 {
			b.Fatal("the queue ran dry mid-benchmark; the measurement would be of an empty scan")
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

func benchStart(b *testing.B, be backend) {
	s := be.open(b)
	fill(b, s, b.N+stepsPerJob, epoch)
	leases := claimN(b, s, b.N)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := s.Start(ctx, leases[i], epoch); err != nil {
			b.Fatalf("Start: %v", err)
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

// benchHeartbeat beats ONE lease b.N times rather than b.N leases once.
//
// That is what actually happens: a step running for its full timeout heartbeats
// every RUNMESH_HEARTBEAT_INTERVAL against the same row, and the interesting
// property of that write is that it touches one row repeatedly — which is the
// access pattern most likely to find a lock or an index problem, and the one a
// scattered write would never produce.
func benchHeartbeat(b *testing.B, be backend) {
	s := be.open(b)
	fill(b, s, stepsPerJob, epoch)
	l := claimN(b, s, 1)[0]
	ctx := context.Background()
	if err := s.Start(ctx, l, epoch); err != nil {
		b.Fatalf("Start: %v", err)
	}

	until := epoch.Add(benchLeaseTTL)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.Heartbeat(ctx, l, epoch, until); err != nil {
			b.Fatalf("Heartbeat: %v", err)
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

func benchFinish(b *testing.B, be backend) {
	s := be.open(b)
	fill(b, s, b.N+stepsPerJob, epoch)
	leases := claimN(b, s, b.N)
	ctx := context.Background()
	for i := range b.N {
		if err := s.Start(ctx, leases[i], epoch); err != nil {
			b.Fatalf("Start: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		err := s.Finish(ctx, runmesh.Outcome{
			Lease:     leases[i],
			State:     runmesh.Succeeded,
			Result:    nullResult,
			StartedAt: epoch,
			EndedAt:   epoch.Add(benchLeaseTTL / 2),
		})
		if err != nil {
			b.Fatalf("Finish: %v", err)
		}
	}
	b.StopTimer()
	reportRate(b, b.N)
}

// BenchmarkClaimUnderContention is the benchmark this project exists to
// justify.
//
// The whole design rests on one bet: that a step can be handed to exactly one
// worker by SELECT ... FOR UPDATE SKIP LOCKED, with no coordinator, no
// partition assignment and no leader — and that the cost of doing so does not
// collapse when replicas are added. internal/storetest already proves the
// CORRECTNESS half, with thirty-two concurrent claimers taking five hundred
// steps and each step coming out exactly once. This measures what that
// exclusivity costs, and whether the answer changes with the number of
// claimers.
//
// The two implementations are expected to behave completely differently here,
// and the difference is the interesting output rather than a flaw in the
// measurement. memstore serialises every claimer on one mutex AND walks every
// job and every step inside it, so more claimers can only ever make it slower —
// which is fine, because its documented job is development and fast tests, not
// a fleet. pgstore's claimers skip each other's locked rows and do not queue,
// so the shape of its curve is the thing worth reading: flat means the bet
// holds, a knee means it does not.
//
// The reported metrics are per-lease rather than per-iteration on purpose. One
// iteration is one lease actually taken, no matter how many round trips it cost
// or which goroutine made them, because a lease taken is the unit of work the
// runtime is made of.
func BenchmarkClaimUnderContention(b *testing.B) {
	for _, be := range backends(b) {
		for _, claimers := range []int{1, 2, 4, 8, 16, 32} {
			b.Run("store="+be.name+"/claimers="+strconv.Itoa(claimers), func(b *testing.B) {
				benchContention(b, be, claimers)
			})
		}
	}
}

func benchContention(b *testing.B, be backend, claimers int) {
	const batch = 4 // the shipped RUNMESH_CLAIM_BATCH shape: a small greedy drain

	s := be.open(b)
	// Headroom for every claimer to over-shoot by a full batch at the end. A
	// claimer that finds the queue empty stops, and a benchmark that ends with
	// half its goroutines measuring empty scans reports a rate that is mostly
	// the cost of finding nothing.
	fill(b, s, b.N+claimers*batch+stepsPerJob, epoch)
	ctx := context.Background()

	var (
		taken  atomic.Int64
		trips  atomic.Int64
		failed atomic.Value // error
		wg     sync.WaitGroup
	)
	target := int64(b.N)

	b.ReportAllocs()
	b.ResetTimer()
	wg.Add(claimers)
	for range claimers {
		go func() {
			defer wg.Done()
			for taken.Load() < target {
				leases, err := s.Claim(ctx, runmesh.ClaimRequest{
					Owner: "bench", Limit: batch, LeaseTTL: benchLeaseTTL, Now: epoch,
				})
				trips.Add(1)
				if err != nil {
					failed.Store(err)
					return
				}
				if len(leases) == 0 {
					// Not an error — Claim returning zero is normal — but with
					// the headroom above it can only mean the queue is gone,
					// and continuing would measure empty scans.
					return
				}
				taken.Add(int64(len(leases)))
			}
		}()
	}
	wg.Wait()
	b.StopTimer()

	if err, ok := failed.Load().(error); ok && err != nil {
		b.Fatalf("Claim: %v", err)
	}
	got := taken.Load()
	if got < target {
		b.Fatalf("claimed %d leases for b.N = %d: the fixture ran dry and the rate would be a lie", got, target)
	}
	reportRate(b, int(got))
	// Leases per round trip says whether the claimers were actually colliding.
	// A value at the batch size means every claimer found a full batch and the
	// contention is notional; a value falling towards one means they are
	// genuinely stepping over each other's locked rows, which is when the
	// claimers-per-second number above starts being about SKIP LOCKED.
	b.ReportMetric(float64(got)/float64(trips.Load()), "leases/trip")
}

// reportRate turns a count of completed units into the rate the README wants to
// quote. b.Elapsed() is the timed interval with every StopTimer stretch already
// removed, which is why none of these benchmarks needs a clock: the expensive
// fixture construction above ResetTimer is genuinely outside the number.
func reportRate(b *testing.B, units int) {
	b.Helper()
	if e := b.Elapsed(); e > 0 {
		b.ReportMetric(float64(units)/e.Seconds(), "ops/sec")
	}
}
