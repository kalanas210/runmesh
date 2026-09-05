package clock_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
)

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

func TestFakeNowAdvancesOnlyWhenTold(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	if got := f.Now(); !got.Equal(epoch) {
		t.Fatalf("Now() = %v, want %v", got, epoch)
	}
	f.Advance(90 * time.Second)
	if got, want := f.Now(), epoch.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("after Advance, Now() = %v, want %v", got, want)
	}
	if got, want := f.Since(epoch), 90*time.Second; got != want {
		t.Fatalf("Since = %v, want %v", got, want)
	}
}

func TestFakeAfterFiresOnlyWhenDue(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	ch := f.After(10 * time.Second)

	f.Advance(9 * time.Second)
	select {
	case at := <-ch:
		t.Fatalf("timer fired early at %v", at)
	default:
	}

	f.Advance(time.Second)
	select {
	case at := <-ch:
		if want := epoch.Add(10 * time.Second); !at.Equal(want) {
			t.Fatalf("fired with %v, want %v", at, want)
		}
	default:
		t.Fatal("timer did not fire once due")
	}
}

// Advance crossing several deadlines must move time to each deadline in turn
// before firing it, not jump to the target and fire everything at once. The
// engine depends on this: the abandon grace is armed relative to the instant a
// step deadline fired, so a fake clock that fired everything at the target
// would make that interval collapse to zero.
func TestFakeAdvanceFiresEachAtItsOwnDeadline(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	// Registered out of deadline order on purpose.
	c := f.After(30 * time.Second)
	a := f.After(10 * time.Second)
	b := f.After(20 * time.Second)

	f.Advance(time.Minute)

	for _, tc := range []struct {
		name string
		ch   <-chan time.Time
		want time.Duration
	}{
		{"a", a, 10 * time.Second},
		{"b", b, 20 * time.Second},
		{"c", c, 30 * time.Second},
	} {
		select {
		case at := <-tc.ch:
			if want := epoch.Add(tc.want); !at.Equal(want) {
				t.Errorf("timer %s fired at %v, want %v", tc.name, at, want)
			}
		default:
			t.Errorf("timer %s did not fire", tc.name)
		}
	}
	if got, want := f.Now(), epoch.Add(time.Minute); !got.Equal(want) {
		t.Errorf("Now() = %v after Advance, want %v", got, want)
	}
}

// TestAdvanceCallbackMayRegisterTimer is the reentrancy regression. The
// worker's heartbeat re-arms a timer from inside the tick it is handling; if
// Advance held the state mutex while firing, that would deadlock the whole
// suite rather than fail one test.
func TestAdvanceCallbackMayRegisterTimer(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	done := make(chan struct{})
	var rearms atomic.Int32

	first := f.After(time.Second)
	go func() {
		defer close(done)
		for range 3 {
			<-first
			rearms.Add(1)
			first = f.After(time.Second) // registers a new timer from the callback path
		}
	}()

	for range 3 {
		if err := f.BlockUntilContext(t.Context(), 1); err != nil {
			t.Fatalf("waiting for the re-armed timer: %v", err)
		}
		f.Advance(time.Second)
	}
	<-done
	if got := rearms.Load(); got != 3 {
		t.Fatalf("re-armed %d times, want 3", got)
	}
}

func TestFakeTickerRepeatsAndDropsUncollectedTicks(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	tk := f.NewTicker(time.Second)
	defer tk.Stop()

	f.Advance(3 * time.Second) // three ticks, nobody collecting
	got := 0
	for {
		select {
		case <-tk.C():
			got++
			continue
		default:
		}
		break
	}
	if got != 1 {
		t.Fatalf("collected %d ticks, want 1 (uncollected ticks must be dropped, as time.Ticker does)", got)
	}

	tk.Stop()
	f.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("ticker fired after Stop")
	default:
	}
	if n := f.Waiters(); n != 0 {
		t.Fatalf("Waiters() = %d after Stop, want 0", n)
	}
}

func TestFakeBlockUntilContextFailsRatherThanHangs(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	_ = f.After(time.Second)

	if err := f.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatalf("one waiter registered, got %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.BlockUntilContext(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong waiter count must return ctx.Err(), got %v", err)
	}
}

func TestFakeWithTimeoutExpiresOnlyOnAdvance(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	ctx, cancel := f.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f.Advance(29 * time.Second)
	if err := ctx.Err(); err != nil {
		t.Fatalf("context expired early: %v", err)
	}
	f.Advance(time.Second)
	<-ctx.Done()
	if got := context.Cause(ctx); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want context.DeadlineExceeded", got)
	}
}

func TestFakeWithTimeoutCancelDeregisters(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	ctx, cancel := f.WithTimeout(t.Context(), time.Minute)
	if n := f.Waiters(); n != 1 {
		t.Fatalf("Waiters() = %d, want 1", n)
	}
	cancel()
	cancel() // must be idempotent
	if n := f.Waiters(); n != 0 {
		t.Fatalf("Waiters() = %d after cancel, want 0", n)
	}
	if got := context.Cause(ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("cause = %v, want context.Canceled", got)
	}
}

func TestFakeSleepHonoursContext(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- f.Sleep(ctx, time.Hour) }()

	if err := f.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatalf("sleep did not register: %v", err)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep returned %v, want context.Canceled", err)
	}
	if n := f.Waiters(); n != 0 {
		t.Fatalf("Waiters() = %d after cancelled sleep, want 0", n)
	}

	errc2 := make(chan error, 1)
	go func() { errc2 <- f.Sleep(t.Context(), time.Second) }()
	if err := f.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatalf("second sleep did not register: %v", err)
	}
	f.Advance(time.Second)
	if err := <-errc2; err != nil {
		t.Fatalf("completed Sleep returned %v, want nil", err)
	}
}

// Concurrent registration and advancing is the shape the engine actually uses:
// N workers arming heartbeats while one goroutine drives time forward.
func TestFakeConcurrentWaitersRace(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)

	const n = 50
	var wg sync.WaitGroup
	var fired atomic.Int32
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-f.After(time.Second)
			fired.Add(1)
		}()
	}
	if err := f.BlockUntilContext(t.Context(), n); err != nil {
		t.Fatalf("waiting for %d waiters: %v", n, err)
	}
	f.Advance(time.Second)
	wg.Wait()
	if got := fired.Load(); got != n {
		t.Fatalf("%d of %d timers fired", got, n)
	}
}

func TestFakeSetMovesBackwardsWithoutFiring(t *testing.T) {
	t.Parallel()
	f := clock.NewFake(epoch)
	ch := f.After(time.Second)

	f.Set(epoch.Add(-time.Hour))
	select {
	case <-ch:
		t.Fatal("timer fired while moving backwards")
	default:
	}
	if got := f.Now(); !got.Equal(epoch.Add(-time.Hour)) {
		t.Fatalf("Now() = %v after backwards Set", got)
	}
}
