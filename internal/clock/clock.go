// Package clock is the only source of time inside internal/.
//
// purity_test.go parses every non-test file under internal/ and fails the
// build on time.Now/Sleep/After/Tick/NewTimer/NewTicker/AfterFunc/Since and on
// context.WithTimeout/WithDeadline (and their Cause variants) outside this
// package and cmd/.
//
// Banning the two context constructors is the load-bearing half. One stray
// context.WithTimeout would create an anonymous deadline that no test can
// drive and that the classifier cannot name — and nothing would fail, because
// the resulting retry still looks valid. Making the rule mechanical is the
// difference between an invariant and a comment.
package clock

import (
	"context"
	"time"
)

// Clock is the injectable interface every package under internal/ uses instead
// of the time package.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker

	// WithTimeout is the method that matters. Per-step deadlines are the
	// feature most likely to be tested with time.Sleep; routing them through
	// the clock is what makes that unnecessary, and what lets Fake.Advance
	// drive a step timeout in a test that finishes in microseconds.
	WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc)

	// Sleep returns ctx.Err() if the context ends first, so no caller can
	// sleep through a cancellation.
	Sleep(ctx context.Context, d time.Duration) error
}

// Ticker mirrors *time.Ticker, narrowed to what RunMesh uses.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// WithWriteDeadline returns a context DETACHED from the parent's cancellation
// and bounded by its own deadline. It is the single sanctioned way to persist
// an outcome for work whose own context was just cancelled.
//
// Using a step's dead context to record that the step was cancelled is the bug
// that leaves cancelled jobs stuck in RUNNING forever. Encoding WithoutCancel
// inside this helper means no call site can forget it, and the purity test
// means no call site can hand-build the wrong thing instead.
func WithWriteDeadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}
