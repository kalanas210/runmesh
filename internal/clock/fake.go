package clock

import (
	"context"
	"sync"
	"time"
)

// Fake is a Clock that advances only when told to. It is not a test-only
// build-tagged file: it is a normal part of the package, because the whole
// design of the engine assumes it exists and every runtime test uses it.
//
// The concurrency shape matters and is deliberate:
//
//   - advanceMu serialises Advance/Set, so the order in which timers fire is
//     total and reproducible.
//   - mu guards the clock's own state and is NEVER held while a waiter fires.
//     A fired callback that immediately registers a new timer — which the
//     worker's heartbeat ticker does on every tick — would otherwise deadlock
//     against itself. TestAdvanceCallbackMayRegisterTimer pins that.
type Fake struct {
	advanceMu sync.Mutex

	mu      sync.Mutex
	now     time.Time
	nextID  uint64
	waiters map[uint64]*waiter
	changed chan struct{} // closed and replaced whenever the waiter set changes
}

type waiter struct {
	id       uint64
	deadline time.Time
	period   time.Duration // non-zero for tickers
	fire     func(at time.Time)
}

// NewFake returns a Fake started at the given instant. Tests should pass a
// fixed, readable time so failure messages are legible.
func NewFake(start time.Time) *Fake {
	return &Fake{
		now:     start,
		waiters: make(map[uint64]*waiter),
		changed: make(chan struct{}),
	}
}

var _ Clock = (*Fake)(nil)

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// Waiters reports how many timers, tickers, sleeps and deadline contexts are
// currently registered. Tests use it to assert that the system under test has
// actually parked before advancing.
func (f *Fake) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

// BlockUntilContext returns once at least n waiters are registered, or when
// ctx ends. Taking a context — tests pass t.Context() — means a wrong n FAILS
// the test with a deadline error instead of hanging it, which is the whole
// difference between a debuggable suite and a mysterious CI timeout.
func (f *Fake) BlockUntilContext(ctx context.Context, n int) error {
	for {
		f.mu.Lock()
		if len(f.waiters) >= n {
			f.mu.Unlock()
			return nil
		}
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Advance moves the clock forward by d, firing every waiter that comes due, in
// deadline order.
func (f *Fake) Advance(d time.Duration) {
	f.advanceMu.Lock()
	defer f.advanceMu.Unlock()
	f.mu.Lock()
	target := f.now.Add(d)
	f.mu.Unlock()
	f.advanceTo(target)
}

// Set moves the clock to an absolute instant, firing anything due. Moving
// backwards is allowed and fires nothing.
func (f *Fake) Set(t time.Time) {
	f.advanceMu.Lock()
	defer f.advanceMu.Unlock()
	f.advanceTo(t)
}

func (f *Fake) advanceTo(target time.Time) {
	for {
		f.mu.Lock()
		if target.Before(f.now) {
			f.now = target
			f.mu.Unlock()
			return
		}
		next := f.earliestDueLocked(target)
		if next == nil {
			f.now = target
			f.mu.Unlock()
			return
		}
		// Move time to the waiter's deadline before firing, so a callback that
		// registers a relative timer measures from the right instant.
		f.now = next.deadline
		at := f.now
		fire := next.fire
		if next.period > 0 {
			next.deadline = next.deadline.Add(next.period)
		} else {
			delete(f.waiters, next.id)
			f.notifyLocked()
		}
		f.mu.Unlock()

		fire(at) // never called while mu is held
	}
}

// earliestDueLocked returns the due waiter with the smallest (deadline, id).
// Breaking ties on the registration id makes firing order total, so a test
// that depends on two timers due at the same instant is reproducible.
func (f *Fake) earliestDueLocked(target time.Time) *waiter {
	var best *waiter
	for _, w := range f.waiters {
		if w.deadline.After(target) {
			continue
		}
		switch {
		case best == nil,
			w.deadline.Before(best.deadline),
			w.deadline.Equal(best.deadline) && w.id < best.id:
			best = w
		}
	}
	return best
}

func (f *Fake) register(d time.Duration, period time.Duration, fire func(time.Time)) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	f.waiters[id] = &waiter{id: id, deadline: f.now.Add(d), period: period, fire: fire}
	f.notifyLocked()
	return id
}

func (f *Fake) unregister(id uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.waiters[id]; ok {
		delete(f.waiters, id)
		f.notifyLocked()
	}
}

// notifyLocked wakes every BlockUntilContext caller. Closing and replacing a
// channel is a broadcast that needs no bookkeeping of who is waiting.
func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.register(d, 0, func(at time.Time) { ch <- at })
	return ch
}

func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	t := &fakeTicker{ch: make(chan time.Time, 1), f: f}
	t.id = f.register(d, d, func(at time.Time) {
		// Same semantics as time.Ticker: a tick nobody collected is dropped
		// rather than queued, so a slow consumer cannot build a backlog.
		select {
		case t.ch <- at:
		default:
		}
	})
	return t
}

type fakeTicker struct {
	ch   chan time.Time
	f    *Fake
	id   uint64
	once sync.Once
}

func (t *fakeTicker) C() <-chan time.Time { return t.ch }

func (t *fakeTicker) Stop() { t.once.Do(func() { t.f.unregister(t.id) }) }

// WithTimeout returns a real, cancellable context whose expiry is driven by
// Advance rather than by wall-clock time.
//
// It cancels with cause context.DeadlineExceeded. ctx.Err() is therefore
// context.Canceled rather than context.DeadlineExceeded — which is harmless
// here precisely because nothing in RunMesh classifies an outcome from
// ctx.Err() or context.Cause(). The worker decides from its own `stop`
// variable, assigned before any cancellation is issued.
func (f *Fake) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	id := f.register(d, 0, func(time.Time) { cancel(context.DeadlineExceeded) })
	var once sync.Once
	return ctx, func() {
		once.Do(func() { f.unregister(id) })
		cancel(context.Canceled)
	}
}

func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	ch := make(chan time.Time, 1)
	id := f.register(d, 0, func(at time.Time) { ch <- at })
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		f.unregister(id)
		return ctx.Err()
	}
}
