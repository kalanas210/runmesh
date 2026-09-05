package memstore

import (
	"sync"
	"sync/atomic"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// subscriber is one live event consumer: a test asserting on the exact
// transition sequence today, the WebSocket fan-out in Week 2.
//
// The channel has exactly ONE closer. Both the unsubscribe function and
// Store.Close can end a subscription, and both go through the same sync.Once,
// so "closed twice" and "closed while publishing" are structurally impossible
// rather than merely unlikely.
type subscriber struct {
	ch   chan runmesh.Event
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

type subscribers struct {
	mu      sync.Mutex
	set     map[*subscriber]struct{}
	dropped atomic.Uint64
}

func newSubscribers() *subscribers {
	return &subscribers{set: make(map[*subscriber]struct{})}
}

// add registers a subscriber and returns it with its unsubscribe function.
//
// buf is the per-subscriber buffer. A subscriber that cannot keep up loses
// events rather than stalling the store: an observability channel must never
// be able to apply backpressure to execution.
func (s *subscribers) add(buf int) (<-chan runmesh.Event, func()) {
	if buf < 1 {
		buf = 1
	}
	sub := &subscriber{ch: make(chan runmesh.Event, buf)}

	s.mu.Lock()
	s.set[sub] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.set, sub)
			s.mu.Unlock()
			sub.close()
		})
	}
}

// publish delivers an event to every subscriber with a NON-BLOCKING send.
//
// Because the send can never block, this lock is never held across a blocking
// operation, which is what makes it safe to call while the store's own mutex
// is held — and holding both is what guarantees subscribers observe events in
// GlobalSeq order rather than in whatever order goroutines happen to wake.
func (s *subscribers) publish(e runmesh.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		select {
		case sub.ch <- e:
		default:
			s.dropped.Add(1)
		}
	}
}

// closeAll ends every subscription. Used by Store.Close so a consumer ranging
// over the channel terminates instead of blocking forever.
func (s *subscribers) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		sub.close()
		delete(s.set, sub)
	}
}

// Dropped counts events that no subscriber buffer had room for. It is exported
// through Store.Dropped so a slow dashboard connection is visible rather than
// silently lossy.
func (s *subscribers) Dropped() uint64 { return s.dropped.Load() }
