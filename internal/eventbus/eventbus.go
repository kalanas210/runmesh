// Package eventbus is the process-local fan-out that turns one store
// subscription into many per-job ones.
//
// WHY IT IS ITS OWN PACKAGE, AND NOT PART OF THE ENGINE. internal/engine's
// package doc fixes the goroutine census at "exactly 2 + Workers, plus one
// transient goroutine per in-flight step". A fan-out goroutine started inside
// that package would contradict a stated design invariant, and nothing would
// mechanically catch it — no test counts goroutines. So the pump lives here,
// is constructed in cmd/server/run.go like every other dependency, and the
// engine's claim stays true by construction.
//
// WHY IT EXISTS AT ALL, GIVEN THAT CORRECTNESS DOES NOT DEPEND ON IT. The
// store's own Subscribe is process-local: on a two-replica deployment behind a
// load balancer each replica sees only the events it wrote itself. A stream
// handler that trusted this bus alone would therefore show one dashboard a
// plausible-but-incomplete timeline, which is worse than polling. So the HTTP
// handler treats the durable per-job timeline as the source of truth and polls
// it on a ticker; this bus is a LATENCY ACCELERATOR that collapses that poll
// interval to sub-millisecond on the replica that happened to write the event.
// Remove it and the stream still delivers every event, one poll later.
//
// Delivery is a non-blocking send with a drop counter, copying the property
// both stores already have: observability may never apply backpressure to
// execution. A drop is not a hole in the client's timeline, because the per-job
// Seq is gap-free — a subscriber that sees Seq jump knows exactly what it
// missed and reads it back from the store.
//
// LISTEN/NOTIFY, when it arrives, drops in HERE as a second producer feeding
// the same fan-out: a second pump calling fanout(). The handler, the wire
// format and the browser client all stay exactly as they are. That is the whole
// reason the accelerator is a separate object rather than a few lines inside
// the handler.
package eventbus

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Source is the slice of a store this package needs, declared HERE by its
// consumer for the same reason engine.Store and httpapi.Store are declared by
// theirs: so internal/eventbus imports neither store package and a test
// satisfies it with a channel and a closure.
//
// Both *memstore.Store and *pgstore.Store already have exactly this method,
// which is why wiring the bus in cmd/server/run.go changed no store file.
type Source interface {
	Subscribe(buf int) (<-chan runmesh.Event, func())
}

// sourceBuffer is how many events the bus itself will hold from the store
// before the store starts dropping on its way IN.
//
// It is generous, and deliberately far larger than any one subscriber's buffer,
// because a drop here is the expensive kind: it is shared by every open stream
// on this process, so one event lost at the source costs every viewer a
// catch-up query, whereas an event lost on a single subscriber's buffer costs
// only that viewer one. The pump does nothing but a map read and N non-blocking
// sends, so it drains this buffer about as fast as the store can fill it; the
// depth is there for a GC pause, not for a sustained rate.
const sourceBuffer = 1024

// Broker fans one store subscription out to per-job subscribers.
//
// The index is BY JOB rather than a single list every subscriber filters,
// because the expected shape is many dashboards each watching one job: a
// store-wide list would wake every open connection for every event in the
// process and let each one discard 99% of them. A map read under an RLock wakes
// only the connections that asked.
type Broker struct {
	src Source
	log *slog.Logger

	mu     sync.RWMutex
	byJob  map[string]map[*sub]struct{}
	unsub  func()
	closed bool

	dropped atomic.Uint64

	startOnce sync.Once
	closeOnce sync.Once
}

// sub is one subscriber's channel.
//
// The channel has exactly ONE closer, structurally: both the unsubscribe
// function and Broker.Close end a subscription and both go through this
// sync.Once, so "closed twice" and "closed while publishing" are impossible
// rather than merely unlikely. That is the same shape internal/memstore's own
// subscriber has, copied on purpose — two fan-outs that disagree about who
// closes a channel is precisely the bug that only shows up during a shutdown.
type sub struct {
	jobID string
	ch    chan runmesh.Event
	once  sync.Once
}

func (s *sub) close() { s.once.Do(func() { close(s.ch) }) }

// New builds a broker over a store. It starts nothing: Start does that, so the
// caller decides when a goroutine begins to exist.
func New(src Source, log *slog.Logger) *Broker {
	if log == nil {
		log = slog.Default()
	}
	return &Broker{
		src:   src,
		log:   log.With("component", "eventbus"),
		byJob: make(map[string]map[*sub]struct{}),
	}
}

// Start subscribes to the store and begins the single pump goroutine.
//
// Exactly one goroutine, and calling Start twice starts no more: the census
// this package exists to protect would be meaningless if the number depended on
// how many times run() happened to call it.
//
// The pump exits on ctx, on Close, or when the store closes its channel — which
// is the store shutting down and is reported to subscribers by closing their
// channels, never by silence.
func (b *Broker) Start(ctx context.Context) {
	b.startOnce.Do(func() {
		live, unsub := b.src.Subscribe(sourceBuffer)

		b.mu.Lock()
		closed := b.closed
		if !closed {
			b.unsub = unsub
		}
		b.mu.Unlock()

		if closed {
			// Close ran before Start did. Undo the subscription rather than
			// leaking it, and start nothing.
			unsub()
			return
		}
		go b.pump(ctx, live)
	})
}

func (b *Broker) pump(ctx context.Context, live <-chan runmesh.Event) {
	for {
		select {
		case <-ctx.Done():
			// The process is going away. Subscribers learn through Close, which
			// the ordered shutdown calls; ending the pump here only stops the
			// fan-out, it does not decide anything on their behalf.
			return
		case e, ok := <-live:
			if !ok {
				// The store closed the channel: it is shutting down. Every
				// subscriber must be told, and told by a CLOSE rather than by
				// no further events, because a stream that reads silence as
				// "this job's timeline ended" would make a rolling restart look
				// to a dashboard like every in-flight job completed.
				b.closeSubscribers()
				return
			}
			b.fanout(e)
		}
	}
}

// fanout delivers one event to every subscriber watching that job.
//
// The send is non-blocking and a failure increments the drop counter. That is
// not a shortcut: this runs on the pump, which is the only consumer of the
// store's own subscriber buffer, so blocking here for one slow HTTP client
// would stall the feed for every other client in the process — and, once the
// store's buffer filled behind it, would start dropping events for everybody
// instead of for the one connection that could not keep up.
//
// Every delivery is a fresh deep copy. The event arrived as one clone from the
// store; handing that same value to N subscribers would give them a shared
// Attrs map and a shared Error pointer.
func (b *Broker) fanout(e runmesh.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.byJob[e.JobID] {
		select {
		case s.ch <- e.Clone():
		default:
			b.dropped.Add(1)
		}
	}
}

// Subscribe returns a channel of this job's events and the function that ends
// the subscription.
//
// buf is the per-subscriber buffer, and a full one drops rather than blocks —
// see fanout. The caller is expected to notice: because Event.Seq is per-job,
// 1-based and gap-free, a subscriber that receives Seq greater than
// lastSeq+1 knows exactly which events it missed and can read them back from
// the durable timeline. That is why dropping is an acceptable policy here and
// would not be on a feed keyed by a cursor with gaps in it.
//
// Subscribing to a closed broker returns an already-closed channel rather than
// an error, so a handler racing a shutdown takes the same code path it takes
// when the store closes underneath it, instead of needing a second one.
func (b *Broker) Subscribe(jobID string, buf int) (<-chan runmesh.Event, func()) {
	if buf < 1 {
		buf = 1
	}
	s := &sub{jobID: jobID, ch: make(chan runmesh.Event, buf)}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		s.close()
		return s.ch, func() {}
	}
	set, ok := b.byJob[jobID]
	if !ok {
		set = make(map[*sub]struct{})
		b.byJob[jobID] = set
	}
	set[s] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.byJob[s.jobID]; ok {
				delete(set, s)
				// The per-job map entry is removed with its last subscriber, so
				// a process that has streamed a million jobs does not keep a
				// million empty maps.
				if len(set) == 0 {
					delete(b.byJob, s.jobID)
				}
			}
			b.mu.Unlock()
			s.close()
		})
	}
}

// Close ends every subscription and stops the pump.
//
// WHERE THIS GOES IN THE ORDERED SHUTDOWN matters more than it looks, and
// cmd/server/run.go states it: after the engine has drained, so no event is
// produced with nobody to consume it, and BEFORE the store closes, so the pump
// is not left reading a channel the store closed underneath it. Putting Close
// in a defer next to the store's is the easy mistake, and it gets the order
// exactly backwards.
//
// It returns an error to satisfy the shape every other closable dependency in
// the wiring has; it never returns a non-nil one.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		unsub := b.unsub
		b.unsub = nil
		b.mu.Unlock()

		// Unsubscribing from the store first closes the pump's own input, so
		// the pump is on its way out before its subscribers vanish underneath
		// it. It is safe either way — every send is non-blocking and every
		// close is behind a sync.Once — but this order means the last thing the
		// pump does is observe a closed channel rather than fan out into a map
		// that is being emptied.
		if unsub != nil {
			unsub()
		}
		b.closeSubscribers()
	})
	return nil
}

func (b *Broker) closeSubscribers() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for jobID, set := range b.byJob {
		for s := range set {
			s.close()
		}
		delete(b.byJob, jobID)
	}
}

// Dropped counts events this broker could not hand to a subscriber because that
// subscriber's buffer was full.
//
// It is separate from the store's own Dropped(), which counts what the store
// could not hand to the BROKER. Two numbers rather than one because they mean
// different things: the store's counter rising says this process could not keep
// up with its own event rate, and this one rising says one HTTP client could
// not keep up with a job.
func (b *Broker) Dropped() uint64 { return b.dropped.Load() }

// Subscribers reports how many live subscriptions exist, for a test and for a
// gauge. It is not on any consumer's interface.
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := 0
	for _, set := range b.byJob {
		n += len(set)
	}
	return n
}
