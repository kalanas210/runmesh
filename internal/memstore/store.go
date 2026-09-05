// Package memstore is the Week-1 store: durable state and the work queue, in
// memory.
//
// It is deliberately written as a faithful simulation of the PostgreSQL store
// that replaces it in Week 2, not as the simplest thing that could work. Every
// method is one unit of work with no transaction object crossing the boundary;
// every timestamp is a parameter rather than a clock read, exactly as a SQL
// bind variable would be; the claim predicate is the same expression as the
// SKIP LOCKED query, written in Go; and every read and write deep-copies, so
// no caller can hold a pointer into store-internal state and pass a test that
// a real database could never reproduce.
//
// The executable statement of that claim is internal/storetest: the same
// conformance suite must pass against pgstore in Week 2, unmodified. If it
// cannot, the interface was wrong — and we find that out from a test run
// rather than from an argument.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Options configures a Store. The zero value is usable.
type Options struct {
	JobEventBuffer    int // per-job event ring capacity
	GlobalEventBuffer int // store-wide event ring capacity
}

// Store is an in-memory implementation of the whole persistence surface.
//
// One mutex guards everything. That is not laziness: a single critical section
// per method is exactly what a single SQL statement gives you in Week 2, so
// any interleaving that is impossible here is impossible there too. Splitting
// the lock would buy throughput this store will never need and would introduce
// interleavings the real store cannot produce.
type Store struct {
	mu     sync.Mutex
	closed bool

	jobs map[string]*runmesh.Job
	idem map[string]string // idempotency key -> job id

	globalSeq   uint64
	global      *eventRing
	jobEvents   map[string]*eventRing
	jobEventCap int

	subs *subscribers

	// ready is a latency hint, never a correctness requirement: a buffered-1
	// channel the dispatcher may be parked on. Losing a hint costs one poll
	// interval; it can never strand a job, because the dispatcher's ticker is
	// the backstop.
	ready chan struct{}
}

// New returns an empty store.
func New(opts Options) *Store {
	if opts.JobEventBuffer < 1 {
		opts.JobEventBuffer = 512
	}
	if opts.GlobalEventBuffer < 1 {
		opts.GlobalEventBuffer = 8192
	}
	return &Store{
		jobs:        make(map[string]*runmesh.Job),
		idem:        make(map[string]string),
		global:      newEventRing(opts.GlobalEventBuffer),
		jobEvents:   make(map[string]*eventRing),
		jobEventCap: opts.JobEventBuffer,
		subs:        newSubscribers(),
		ready:       make(chan struct{}, 1),
	}
}

// Close marks the store closed. It is a STATE CHANGE, not a teardown: nothing
// is freed, and every subsequent call returns ErrClosed rather than panicking,
// so a worker still settling an outcome during shutdown fails cleanly. A
// PostgreSQL pool behaves the same way.
func (s *Store) Close() error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already {
		return nil
	}
	s.subs.closeAll()
	return nil
}

// Ping reports whether the store is usable. It backs GET /api/v1/ready.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.ErrClosed
	}
	return nil
}

// Ready is the dispatcher's wake-up hint. A nil channel would be safe here too
// — nil blocks forever in select and the ticker carries the load — which is
// exactly why a PostgreSQL implementation may return nil until LISTEN/NOTIFY
// is wired up in Week 2.
func (s *Store) Ready() <-chan struct{} { return s.ready }

// signalReady is a non-blocking send performed AFTER the mutex is released, so
// the hint can never be sent while holding a lock.
func (s *Store) signalReady() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// CreateJob persists a validated job.
//
// When idemKey is non-empty and has been seen before, the pre-existing job is
// returned together with ErrDuplicate. The handler turns that into a 200
// rather than a second job, which is what makes a client's retry of a
// submission safe.
func (s *Store) CreateJob(ctx context.Context, j *runmesh.Job, idemKey string) (*runmesh.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if j == nil || j.ID == "" {
		return nil, fmt.Errorf("memstore: CreateJob requires a job with an id")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}

	if idemKey != "" {
		if id, ok := s.idem[idemKey]; ok {
			return s.jobs[id].Clone(), runmesh.ErrDuplicate
		}
	}
	if _, exists := s.jobs[j.ID]; exists {
		return nil, runmesh.ErrConflict
	}

	stored := j.Clone()
	stored.IdempotencyKey = idemKey
	s.jobs[stored.ID] = stored
	if idemKey != "" {
		s.idem[idemKey] = stored.ID
	}
	s.appendEventLocked(stored, runmesh.Event{
		Type:  runmesh.JobCreated,
		At:    stored.CreatedAt,
		State: stored.State,
		Attrs: map[string]any{"steps": len(stored.Steps), "name": stored.Name},
	})

	// Deferred rather than called inline so the hint is sent after the mutex is
	// released, without duplicating the unlock on every return path.
	defer s.signalReady()
	return stored.Clone(), nil
}

// Job returns a deep copy of one job.
func (s *Store) Job(ctx context.Context, id string) (*runmesh.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}
	j, ok := s.jobs[id]
	if !ok {
		return nil, runmesh.ErrNotFound
	}
	return j.Clone(), nil
}

// ListJobs returns a page of jobs, newest first.
//
// Ordering is by id descending. That works because ids are k-sortable, which
// makes keyset pagination need no tie-breaker column and no OFFSET — the same
// query shape as the Week-2 index scan. Sorting on read is O(n log n) and
// perfectly adequate for an in-memory store whose entire lifetime is one
// process; PostgreSQL does it with the primary-key index.
func (s *Store) ListJobs(ctx context.Context, f runmesh.JobFilter) (runmesh.JobPage, error) {
	if err := ctx.Err(); err != nil {
		return runmesh.JobPage{}, err
	}
	limit := f.Limit
	if limit < 1 {
		limit = 50
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runmesh.JobPage{}, runmesh.ErrClosed
	}

	want := make(map[runmesh.State]bool, len(f.States))
	for _, st := range f.States {
		want[st] = true
	}

	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))

	var page runmesh.JobPage
	for _, id := range ids {
		if f.Cursor != "" && id >= f.Cursor {
			continue
		}
		j := s.jobs[id]
		if len(want) > 0 && !want[j.State] {
			continue
		}
		if len(page.Jobs) == limit {
			page.NextCursor = page.Jobs[len(page.Jobs)-1].ID
			break
		}
		page.Jobs = append(page.Jobs, j.Clone())
	}
	return page, nil
}

// RequestCancel sets the cancel flag and cancels every step that has not
// started.
//
// It deliberately does NOT touch SCHEDULED or RUNNING steps: a worker owns
// those, and they learn to stop through their next heartbeat. That is the only
// mechanism that still works in Week 2, when the cancelling API call may land
// on a different replica from the one executing the step.
//
// The job therefore stays RUNNING until those steps drain, and the API returns
// the honest current state rather than a comfortable lie about being stopped.
func (s *Store) RequestCancel(ctx context.Context, id string, reason runmesh.CancelReason, now time.Time) (*runmesh.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, runmesh.ErrClosed
	}

	j, ok := s.jobs[id]
	if !ok {
		return nil, runmesh.ErrNotFound
	}
	if j.State.Terminal() {
		return j.Clone(), runmesh.ErrConflict
	}
	if j.CancelRequestedAt != nil {
		return j.Clone(), nil // idempotent: cancelling twice is not an error
	}

	at := now
	j.CancelRequestedAt = &at
	j.CancelReason = reason
	s.appendEventLocked(j, runmesh.Event{
		Type:  runmesh.JobCancelRequested,
		At:    now,
		State: j.State,
		Attrs: map[string]any{"reason": string(reason)},
	})
	s.reconcileJobLocked(j, now)

	defer s.signalReady()
	return j.Clone(), nil
}

// QueueDepth counts the steps that are claimable right now. It is the same
// predicate Claim uses, counted instead of taken, which is what keeps
// admission control honest as the predicate evolves.
func (s *Store) QueueDepth(ctx context.Context, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, runmesh.ErrClosed
	}

	n := 0
	for _, j := range s.jobs {
		for _, step := range j.Steps {
			if claimable(j, step, now, nil) {
				n++
			}
		}
	}
	return n, nil
}

// Stats is a snapshot for GET /api/v1/ready and, in Week 2, for the metrics
// exporter.
type Stats struct {
	Jobs          int
	QueueDepth    int
	ActiveSteps   int
	EventsWritten uint64
	Dropped       uint64
}

// Snapshot returns store-level counters.
func (s *Store) Snapshot(now time.Time) Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Jobs: len(s.jobs), EventsWritten: s.globalSeq, Dropped: s.subs.Dropped()}
	for _, j := range s.jobs {
		for _, step := range j.Steps {
			if step.State.Active() {
				st.ActiveSteps++
			}
			if claimable(j, step, now, nil) {
				st.QueueDepth++
			}
		}
	}
	return st
}

// lookupLocked resolves the job and step a lease refers to, discriminating the
// three failure modes callers branch on. Getting this discrimination right is
// what stops a worker whose lease was stolen from clobbering the new holder's
// result: ErrLeaseLost means "write nothing at all", while ErrConflict means
// "the state moved on".
func (s *Store) lookupLocked(l runmesh.Lease) (*runmesh.Job, *runmesh.Step, error) {
	j, ok := s.jobs[l.JobID]
	if !ok {
		return nil, nil, runmesh.ErrNotFound
	}
	step := j.Step(l.StepID)
	if step == nil {
		return nil, nil, runmesh.ErrNotFound
	}
	if step.LeaseID == "" || step.LeaseID != l.ID {
		return nil, nil, fmt.Errorf("%w: step %s/%s", runmesh.ErrLeaseLost, l.JobID, l.StepID)
	}
	return j, step, nil
}

// stateGuard rejects a transition the state machine does not allow. It is the
// third of the three redundant gates: the guarded write is the real
// enforcement, CanStep is data a table test walks, and this is where the two
// meet on every single mutation.
func stateGuard(step *runmesh.Step, to runmesh.State) error {
	if !runmesh.CanStep(step.State, to) {
		return fmt.Errorf("%w: step is %s, cannot move to %s", runmesh.ErrConflict, step.State, to)
	}
	return nil
}
