package eventbus

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// These tests are IN-PACKAGE on purpose, which is unusual here and worth the
// sentence. The property this package has to hold is "a slow subscriber loses
// events instead of stalling the pump", and observed from outside the package
// that property is a race between a goroutine and an assertion: you send an
// event, you do not know when the pump has finished fanning it out, and the
// only honest ways to find out are a sleep or a second synchronising send that
// changes what is being measured. Calling fanout directly makes the whole thing
// straight-line code with no goroutine at all, which is exactly what the rest
// of this codebase does with engine.RunOnce. The pump gets its own cases below,
// where a channel receive is a real synchronisation point rather than a guess.

// fakeSource is a store, as far as this package is concerned: one channel and
// one unsubscribe. Both real stores are this shape plus a database.
type fakeSource struct {
	ch chan runmesh.Event
	// buf records what the broker asked for, so a test can pin that the pump
	// takes a generous buffer from the store rather than an unbuffered one.
	buf   atomic.Int64
	calls atomic.Int64
	unsub atomic.Int64
}

func (s *fakeSource) Subscribe(buf int) (<-chan runmesh.Event, func()) {
	s.buf.Store(int64(buf))
	s.calls.Add(1)
	return s.ch, func() { s.unsub.Add(1) }
}

func newBroker(t *testing.T) (*Broker, *fakeSource) {
	t.Helper()
	src := &fakeSource{ch: make(chan runmesh.Event)}
	b := New(src, slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { _ = b.Close() })
	return b, src
}

func event(jobID string, seq uint64) runmesh.Event {
	return runmesh.Event{
		JobID: jobID,
		Seq:   seq,
		Type:  runmesh.StepStarted,
		Attrs: map[string]any{"seq": seq},
	}
}

// recv takes one event off a subscription, failing rather than hanging if the
// channel turned out to be closed. It never blocks in a passing test, because
// every value it reads is one a fanout call already delivered.
func recv(t *testing.T, ch <-chan runmesh.Event) runmesh.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("the subscription closed while an event was expected")
		}
		return e
	default:
		t.Fatal("the subscription is empty; the fan-out did not deliver")
		return runmesh.Event{}
	}
}

func TestBroker(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"every subscriber of a job receives the event", testFanoutReachesEverySubscriber},
		{"a subscription sees its own job and nothing else", testSubscriptionIsPerJob},
		{"each subscriber gets its own copy of the event", testDeliveryIsACopy},
		{"a full buffer drops and counts instead of blocking", testFullBufferDropsRatherThanBlocking},
		{"unsubscribe is idempotent and closes exactly once", testUnsubscribeIsIdempotent},
		{"unsubscribing the last viewer forgets the job", testTheJobIndexDoesNotLeak},
		{"Close terminates every ranging consumer", testCloseTerminatesEveryConsumer},
		{"the store closing is reported as a close, not as silence", testStoreCloseClosesSubscribers},
		{"Start subscribes once however often it is called", testStartIsOnce},
		{"subscribing to a closed broker yields a closed channel", testSubscribeAfterCloseIsClosed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t)
		})
	}
}

func testFanoutReachesEverySubscriber(t *testing.T) {
	b, _ := newBroker(t)

	a, unsubA := b.Subscribe("job_1", 4)
	defer unsubA()
	c, unsubC := b.Subscribe("job_1", 4)
	defer unsubC()

	b.fanout(event("job_1", 7))

	if got := recv(t, a).Seq; got != 7 {
		t.Errorf("first subscriber saw seq %d, want 7", got)
	}
	if got := recv(t, c).Seq; got != 7 {
		t.Errorf("second subscriber saw seq %d, want 7", got)
	}
	if n := b.Subscribers(); n != 2 {
		t.Errorf("Subscribers() = %d, want 2", n)
	}
}

// testSubscriptionIsPerJob pins the reason the index is a map rather than one
// list every connection filters: a dashboard watching one job must not be woken
// for every event in the process.
func testSubscriptionIsPerJob(t *testing.T) {
	b, _ := newBroker(t)

	mine, unsub := b.Subscribe("job_mine", 4)
	defer unsub()

	b.fanout(event("job_theirs", 1))
	b.fanout(event("job_mine", 2))

	if got := recv(t, mine).JobID; got != "job_mine" {
		t.Fatalf("received an event for %q, want job_mine only", got)
	}
	if len(mine) != 0 {
		t.Errorf("%d further events are queued; the other job's event leaked", len(mine))
	}
}

// testDeliveryIsACopy: the event arrives from the store as one clone, and
// handing that same value to N subscribers would give them a shared Attrs map —
// which two connections reading concurrently is a data race, and one connection
// mutating is a bug in the other's timeline.
func testDeliveryIsACopy(t *testing.T) {
	b, _ := newBroker(t)

	a, unsubA := b.Subscribe("job_1", 2)
	defer unsubA()
	c, unsubC := b.Subscribe("job_1", 2)
	defer unsubC()

	original := event("job_1", 1)
	b.fanout(original)

	first, second := recv(t, a), recv(t, c)
	first.Attrs["seq"] = "mutated"

	if second.Attrs["seq"] == "mutated" {
		t.Error("two subscribers share one Attrs map")
	}
	if original.Attrs["seq"] == "mutated" {
		t.Error("a subscriber holds the publisher's Attrs map")
	}
}

// testFullBufferDropsRatherThanBlocking is the property the whole design rests
// on: the pump is the only consumer of the store's own subscriber buffer, so
// blocking here for one slow HTTP client would stall the feed for every other
// client in the process and then start dropping events for all of them.
//
// The assertion is deliberately BOTH halves — the publisher returned, AND the
// buffer is still full — because a drop that silently made room would be a
// different bug with the same counter.
func testFullBufferDropsRatherThanBlocking(t *testing.T) {
	b, _ := newBroker(t)

	slow, unsub := b.Subscribe("job_1", 1)
	defer unsub()

	b.fanout(event("job_1", 1)) // fills the buffer
	b.fanout(event("job_1", 2)) // must be dropped
	b.fanout(event("job_1", 3)) // and so must this one

	// Reaching this line at all is the "does not block" half of the assertion:
	// a blocking send would have parked this goroutine on the second call and
	// the test would never get here.
	if n := b.Dropped(); n != 2 {
		t.Errorf("Dropped() = %d, want 2", n)
	}
	if len(slow) != 1 {
		t.Errorf("the subscriber buffer holds %d events, want 1: a drop must not "+
			"make room by discarding what was already delivered", len(slow))
	}
	if got := recv(t, slow).Seq; got != 1 {
		t.Errorf("the retained event is seq %d, want 1 (the oldest, not the newest)", got)
	}
}

// testUnsubscribeIsIdempotent: a handler's `defer unsub()` and a shutdown's
// Close can both end the same subscription, and a channel closed twice panics.
// One sync.Once shared by both is what makes that structurally impossible
// rather than merely unlikely.
func testUnsubscribeIsIdempotent(t *testing.T) {
	b, _ := newBroker(t)

	ch, unsub := b.Subscribe("job_1", 1)
	unsub()
	unsub()
	unsub()
	if err := b.Close(); err != nil {
		t.Fatalf("Close after unsubscribe: %v", err)
	}

	if _, ok := <-ch; ok {
		t.Fatal("the subscription is still open after unsubscribing")
	}
	// A second read from a closed channel is legal; a second CLOSE is not, and
	// the three calls above plus Close would have panicked if it happened.
	if _, ok := <-ch; ok {
		t.Fatal("a closed channel yielded a value")
	}
}

// testTheJobIndexDoesNotLeak: a process that has streamed a million jobs must
// not keep a million empty maps, so the per-job entry goes with its last viewer.
func testTheJobIndexDoesNotLeak(t *testing.T) {
	b, _ := newBroker(t)

	_, unsubA := b.Subscribe("job_1", 1)
	_, unsubB := b.Subscribe("job_1", 1)

	unsubA()
	b.mu.RLock()
	stillIndexed := len(b.byJob) == 1
	b.mu.RUnlock()
	if !stillIndexed {
		t.Fatal("the job was forgotten while a viewer was still attached")
	}

	unsubB()
	b.mu.RLock()
	remaining := len(b.byJob)
	b.mu.RUnlock()
	if remaining != 0 {
		t.Errorf("byJob holds %d entries after the last viewer left, want 0", remaining)
	}
	if n := b.Subscribers(); n != 0 {
		t.Errorf("Subscribers() = %d, want 0", n)
	}
}

// testCloseTerminatesEveryConsumer: a stream handler ranges over its channel,
// so shutdown has to end the range. A Close that only stopped publishing would
// leave every open connection parked on a receive for ever, and the process
// would never exit.
func testCloseTerminatesEveryConsumer(t *testing.T) {
	b, _ := newBroker(t)

	const viewers = 3
	var wg sync.WaitGroup
	for i := 0; i < viewers; i++ {
		ch, _ := b.Subscribe("job_1", 1)
		wg.Add(1)
		go func(ch <-chan runmesh.Event) {
			defer wg.Done()
			for range ch {
			}
		}(ch)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Waiting is the assertion. Every consumer ends, or this test hangs and the
	// suite's own timeout names the case that failed to terminate.
	wg.Wait()

	if err := b.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// testStoreCloseClosesSubscribers pins the one interpretation a stream handler
// must never make. When the store closes its channel the process is shutting
// down; if that reached a dashboard as silence, or as end-of-timeline, a rolling
// restart would look like every in-flight job had completed.
func testStoreCloseClosesSubscribers(t *testing.T) {
	b, src := newBroker(t)
	b.Start(t.Context())

	ch, _ := b.Subscribe("job_1", 1)
	close(src.ch)

	if _, ok := <-ch; ok {
		t.Fatal("the subscription delivered an event after the store closed")
	}
	if n := b.Subscribers(); n != 0 {
		t.Errorf("Subscribers() = %d after the store closed, want 0", n)
	}
}

// testStartIsOnce: this package exists so the engine's stated goroutine census
// stays true, and a census that depended on how many times run() happened to
// call Start would be worth nothing.
func testStartIsOnce(t *testing.T) {
	b, src := newBroker(t)

	b.Start(t.Context())
	b.Start(t.Context())
	b.Start(t.Context())

	if n := src.calls.Load(); n != 1 {
		t.Errorf("the store was subscribed to %d times, want 1", n)
	}
	if got := src.buf.Load(); got != sourceBuffer {
		t.Errorf("the pump asked the store for a buffer of %d, want %d",
			got, sourceBuffer)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := src.unsub.Load(); n != 1 {
		t.Errorf("the store subscription was released %d times, want 1", n)
	}
}

// testSubscribeAfterCloseIsClosed: a handler racing a shutdown takes the same
// code path it takes when the store closes underneath it, rather than needing a
// second one for a broker that is merely early.
func testSubscribeAfterCloseIsClosed(t *testing.T) {
	b, _ := newBroker(t)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ch, unsub := b.Subscribe("job_1", 1)
	defer unsub()

	if _, ok := <-ch; ok {
		t.Fatal("a closed broker handed out a live subscription")
	}
	// And the fan-out still has to be safe to call, because the pump may be one
	// event behind the close.
	b.fanout(event("job_1", 1))
}

// testStartAfterCloseSubscribesNothing is not in the table because it asserts
// the reverse ordering of the pair above, and reads better next to it.
func TestStartAfterCloseLeaksNoSubscription(t *testing.T) {
	t.Parallel()

	b, src := newBroker(t)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b.Start(t.Context())

	// Start still subscribes — it has to, to discover the broker is closed
	// without holding the lock across the store call — but it must hand the
	// subscription straight back rather than leak it and start a pump nobody
	// will ever stop.
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("the store was subscribed to %d times, want 1", n)
	}
	if n := src.unsub.Load(); n != 1 {
		t.Errorf("the subscription was released %d times, want 1: Start after "+
			"Close must not leave the store feeding a pump that does not exist", n)
	}
}
