package engine

import (
	"math"
	"math/rand/v2"
	"time"
)

// Backoff is a pure value, and what it does NOT hold matters as much as what
// it does: no *rand.Rand and no mutex. It can therefore be copied freely, go
// vet's copylocks check can never fire on it, and there is no shared RNG for
// concurrent retries to race on. Rand is nil in production and resolves to
// math/rand/v2's global source, which is goroutine-safe.
type Backoff struct {
	Base   time.Duration // 1s
	Max    time.Duration // 60s
	Factor float64       // 2.0 gives 1s, 2s, 4s, 8s
	Jitter float64       // 0.2 spreads the delay over [d*0.8, d]
	Rand   func() float64
}

// Delay is pure and total.
//
// failures is the count BEFORE this failure, so Delay(0) == Base.
//
// Growth is computed in float64 with the cap applied before conversion,
// because Base<<40 overflows int64 and would come back as a negative duration
// — a step scheduled for the past, retried instantly, for ever. Writing the
// comparison as !(d < max) rather than d > max also clamps +Inf and NaN, which
// is what a misconfigured Factor produces.
func (b Backoff) Delay(failures int) time.Duration {
	if failures < 0 {
		failures = 0
	}
	factor := b.Factor
	if factor < 1 {
		factor = 1
	}
	d := float64(b.Base) * math.Pow(factor, float64(failures))
	if !(d < float64(b.Max)) {
		d = float64(b.Max)
	}
	if b.Jitter > 0 {
		r := b.Rand
		if r == nil {
			r = rand.Float64
		}
		// Jitter subtracts, never adds: the delay stays within [d*(1-j), d], so
		// a configured Max is a real ceiling rather than an average.
		d *= 1 - b.Jitter*r()
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}
