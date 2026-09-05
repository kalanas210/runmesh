package engine_test

import (
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
)

func noJitter() engine.Backoff {
	return engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}
}

func TestBackoffLadder(t *testing.T) {
	t.Parallel()
	b := noJitter()

	// failures is the count BEFORE this failure, so Delay(0) is the first wait.
	for i, want := range []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, time.Minute, time.Minute,
	} {
		if got := b.Delay(i); got != want {
			t.Errorf("Delay(%d) = %s, want %s", i, got, want)
		}
	}
}

// TestBackoffOverflow is the reason growth is computed in float64 with the cap
// applied before conversion. Base<<40 overflows int64, and a negative duration
// would mean "retry in the past" — a step retried instantly, for ever.
func TestBackoffOverflow(t *testing.T) {
	t.Parallel()
	b := noJitter()
	for _, failures := range []int{40, 100, 1000, 1 << 20} {
		got := b.Delay(failures)
		if got != time.Minute {
			t.Errorf("Delay(%d) = %s, want the Max of %s", failures, got, time.Minute)
		}
		if got < 0 {
			t.Fatalf("Delay(%d) = %s: a negative backoff retries in the past", failures, got)
		}
	}
}

func TestBackoffHandlesDegenerateConfig(t *testing.T) {
	t.Parallel()
	// A negative failure count must not produce a negative delay.
	if got := noJitter().Delay(-5); got != time.Second {
		t.Errorf("Delay(-5) = %s, want the base delay", got)
	}
	// A factor below 1 would shrink the delay on every failure, which defeats
	// the point of backoff; it is clamped instead.
	flat := engine.Backoff{Base: 2 * time.Second, Max: time.Minute, Factor: 0.5}
	if got := flat.Delay(3); got != 2*time.Second {
		t.Errorf("Delay(3) with factor 0.5 = %s, want a flat base delay", got)
	}
}

// TestBackoffJitterStaysWithinBounds: jitter only ever subtracts, so a
// configured Max is a real ceiling rather than an average.
func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	t.Parallel()
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 1} {
		b := engine.Backoff{
			Base: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.2,
			Rand: func() float64 { return r },
		}
		got := b.Delay(2) // undelayed: 4s
		lo := time.Duration(float64(4*time.Second) * 0.8)
		hi := 4 * time.Second
		if got < lo || got > hi {
			t.Errorf("with rand=%v, Delay(2) = %s, want within [%s, %s]", r, got, lo, hi)
		}
	}

	// The ceiling holds under jitter too.
	b := engine.Backoff{
		Base: time.Second, Max: 10 * time.Second, Factor: 2, Jitter: 0.5,
		Rand: func() float64 { return 0 },
	}
	if got := b.Delay(20); got > 10*time.Second {
		t.Errorf("Delay(20) = %s, above the Max of 10s", got)
	}
}

// TestBackoffIsCopyable pins the reason Backoff holds no *rand.Rand and no
// mutex: it is passed by value on every classification, from every worker.
func TestBackoffIsCopyable(t *testing.T) {
	t.Parallel()
	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.1}
	copies := make([]engine.Backoff, 8)
	for i := range copies {
		copies[i] = b
	}
	done := make(chan time.Duration, len(copies))
	for _, c := range copies {
		go func() { done <- c.Delay(3) }()
	}
	for range copies {
		if d := <-done; d <= 0 {
			t.Fatalf("concurrent Delay returned %s", d)
		}
	}
}
