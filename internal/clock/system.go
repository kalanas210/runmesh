package clock

import (
	"context"
	"time"
)

// System returns a Clock backed by the standard library.
//
// Inside a testing/synctest bubble the standard library's own time is already
// fake, so this transparently becomes a controllable clock there too — which
// is why the goroutine-leak and idle-parking tests need no injection at all.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time                  { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration { return time.Since(t) }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (systemClock) NewTicker(d time.Duration) Ticker { return systemTicker{time.NewTicker(d)} }

func (systemClock) WithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type systemTicker struct{ t *time.Ticker }

func (s systemTicker) C() <-chan time.Time { return s.t.C }
func (s systemTicker) Stop()               { s.t.Stop() }
