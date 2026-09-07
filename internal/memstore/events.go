package memstore

import (
	"context"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// eventRing is a fixed-capacity FIFO. This store keeps events in memory, so the
// timeline has to be bounded: an unbounded log would make a long-running job a
// memory leak. Eviction is visible to callers (EventPage.Truncated) rather
// than silent, and pgstore has no such limit — which is why nothing smarter
// was ever built here.
type eventRing struct {
	buf   []runmesh.Event
	start int
	n     int
}

func newEventRing(capacity int) *eventRing {
	if capacity < 1 {
		capacity = 1
	}
	return &eventRing{buf: make([]runmesh.Event, capacity)}
}

func (r *eventRing) push(e runmesh.Event) {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = e
		r.n++
		return
	}
	r.buf[r.start] = e
	r.start = (r.start + 1) % len(r.buf)
}

func (r *eventRing) at(i int) runmesh.Event { return r.buf[(r.start+i)%len(r.buf)] }

func (r *eventRing) len() int { return r.n }

// appendEventLocked assigns both cursors, stores the event in the per-job and
// store-wide rings, and publishes it.
//
// Both sequence numbers are assigned here, inside the same critical section as
// the state change that produced them. That is the whole point, and pgstore
// does the same inside the transaction that holds the job row: two concurrent
// appends to one job cannot both read the same counter and
// produce a duplicate or a gap. A gap-free per-job sequence is what makes it
// usable as a WebSocket resume cursor.
//
// The caller must hold s.mu.
func (s *Store) appendEventLocked(j *runmesh.Job, e runmesh.Event) {
	s.globalSeq++
	j.EventSeq++
	e.GlobalSeq = s.globalSeq
	e.Seq = j.EventSeq
	e.JobID = j.ID

	ring, ok := s.jobEvents[j.ID]
	if !ok {
		ring = newEventRing(s.jobEventCap)
		s.jobEvents[j.ID] = ring
	}
	ring.push(e)
	s.global.push(e)

	// Publishing while s.mu is held is deliberate: the send is non-blocking,
	// so it cannot deadlock, and it guarantees subscribers see events in
	// GlobalSeq order instead of in goroutine-wakeup order.
	s.subs.publish(e)
}

// JobEvents returns one page of a job's timeline, starting after afterSeq.
func (s *Store) JobEvents(ctx context.Context, jobID string, afterSeq uint64, limit int) (runmesh.EventPage, error) {
	if err := ctx.Err(); err != nil {
		return runmesh.EventPage{}, err
	}
	if limit < 1 {
		limit = 200
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.EventPage{}, runmesh.ErrClosed
	}
	if _, ok := s.jobs[jobID]; !ok {
		return runmesh.EventPage{}, runmesh.ErrNotFound
	}

	page := runmesh.EventPage{NextAfter: afterSeq}
	ring, ok := s.jobEvents[jobID]
	if !ok || ring.len() == 0 {
		return page, nil
	}

	oldest := ring.at(0).Seq
	page.OldestSeq = oldest
	// The caller asked to resume from a point this ring has already evicted.
	// Saying so beats returning a plausible-looking page with a hole in it.
	//
	// Written as a subtraction rather than afterSeq+1 < oldest: Seq is uint64,
	// so an absurd cursor would wrap to zero and report a gap on a timeline
	// that has none.
	page.Truncated = oldest > 1 && afterSeq < oldest-1

	for i := range ring.len() {
		e := ring.at(i)
		if e.Seq <= afterSeq {
			continue
		}
		if len(page.Events) == limit {
			break
		}
		page.Events = append(page.Events, e.Clone())
		page.NextAfter = e.Seq
	}
	return page, nil
}

// TailEvents returns store-wide events after a global cursor. It is what lets
// a reconnecting dashboard catch up on everything it missed, across all jobs,
// before switching to the live subscription.
func (s *Store) TailEvents(ctx context.Context, afterGlobal uint64, limit int) ([]runmesh.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 200
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}

	var out []runmesh.Event
	for i := range s.global.len() {
		e := s.global.at(i)
		if e.GlobalSeq <= afterGlobal {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, e.Clone())
	}
	return out, nil
}

// Subscribe returns a live event channel and the function that ends the
// subscription. Events are dropped rather than blocking the store when the
// consumer falls behind; Dropped reports how many.
func (s *Store) Subscribe(buf int) (<-chan runmesh.Event, func()) {
	return s.subs.add(buf)
}

// Dropped counts events no subscriber had buffer room for.
func (s *Store) Dropped() uint64 { return s.subs.Dropped() }
