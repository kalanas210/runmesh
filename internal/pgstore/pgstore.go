// Package pgstore is the durable store: PostgreSQL as both the state of record
// and the work queue.
//
// It replaces internal/memstore in production, and it must pass
// internal/storetest — the same conformance suite, unmodified. That was the
// Week-1 claim; this package is the executable proof of it.
//
// # The lock protocol
//
// Every mutation takes the JOB row first, and only then touches step rows.
// Claim and the lease sweep get there via `FOR UPDATE OF j SKIP LOCKED`, which
// locks the job and skips jobs another transaction already holds; every
// single-job method takes it with a plain `SELECT ... FOR UPDATE`.
//
// That uniformity is the whole design, and the alternative is a deadlock rather
// than a slowdown. Claim naturally wants to lock step rows (they are what it
// selects) and then the job row (its rollup writes it). Finish naturally wants
// the job row and then a step row. Two transactions taking two locks in
// opposite orders is the textbook ABBA deadlock, and it would appear only under
// production concurrency: exactly the class of bug that is invisible in
// development. One lock, taken first, everywhere, makes the cycle unbuildable.
//
// The single exception is Heartbeat, which takes no job lock at all. It is the
// hottest write in the system — every running step, every few seconds — and it
// only ever extends a lease on a step a worker already owns, which is a state
// Claim never selects and the rollup never reads. Keeping it out of the lock
// graph costs a stale `jobs.updated_at` and buys a heartbeat that cannot queue
// behind a job's terminal write.
//
// # Read-modify-write, not clever SQL
//
// With the job row locked, each method loads the job and its steps, applies the
// SAME pure policy functions memstore applies (internal/jobstate), and writes
// back the rows whose version changed. The transition rules are therefore not
// reimplemented in SQL: they cannot drift between the two stores, because there
// is only one copy of them.
//
// What IS real SQL is the part that has to be: the readiness predicate, run as
// SKIP LOCKED against real rows under real contention.
package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver, registered as "pgx"

	"github.com/kalanas210/runmesh/internal/jobstate"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Options configures a Store. The zero value is usable with New.
type Options struct {
	// MaxOpenConns bounds the pool. It must exceed the worker count, or a
	// drain can deadlock: every worker holding a connection to settle its
	// outcome while the dispatcher waits for one to claim more work.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration

	// ConnectTimeout bounds the initial Ping. A store that cannot be reached
	// should fail the boot loudly rather than hang holding a listener.
	ConnectTimeout time.Duration

	// Migrate applies pending migrations at Open. On by default in Open; a
	// deployment that runs migrations as a separate step turns it off.
	Migrate bool

	Log *slog.Logger
}

// Store implements the full persistence surface over PostgreSQL.
type Store struct {
	db     *sql.DB
	ownsDB bool
	log    *slog.Logger

	// closed is checked at the top of every method so a caller gets
	// runmesh.ErrClosed — the sentinel every other store returns — rather than
	// a driver-specific "sql: database is closed".
	closed atomic.Bool
	subs   *subscribers

	// ready is the dispatcher's wake-up hint, and it is PROCESS-LOCAL: it
	// wakes the dispatcher in this replica only. That is fine, and the
	// interface says so — the hint is a latency optimisation and the
	// dispatcher's ticker is the correctness backstop. Making it cross-replica
	// means LISTEN/NOTIFY, which needs a dedicated connection outside the
	// database/sql pool; it is deferred to Week 6 with the WebSocket fan-out
	// that needs the same machinery.
	ready chan struct{}
}

// Open connects to PostgreSQL, verifies the connection, and (by default)
// applies pending migrations.
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: opening the database: %w", err)
	}

	s, err := New(db, opts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.ownsDB = true

	timeout := opts.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	// The one-shot connect probe at boot, before any Clock exists to inject and
	// before any step can be running: not a deadline the runtime has to be able
	// to name, nor one a test has to be able to drive.
	pctx, cancel := context.WithTimeout(ctx, timeout) // clock:allow: boot connect probe
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pgstore: connecting to the database: %w", err)
	}

	if opts.Migrate {
		if _, err := Migrate(ctx, db, s.log); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// New wraps an existing pool. It exists so a caller can supply its own
// *sql.DB — a different driver, a proxy, an instrumented pool — without this
// package deciding how the connection is made.
func New(db *sql.DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("pgstore: a *sql.DB is required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.MaxOpenConns > 0 {
		db.SetMaxOpenConns(opts.MaxOpenConns)
	}
	if opts.MaxIdleConns > 0 {
		db.SetMaxIdleConns(opts.MaxIdleConns)
	}
	if opts.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(opts.ConnMaxLifetime)
	}
	if opts.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(opts.ConnMaxIdleTime)
	}
	return &Store{
		db:    db,
		log:   opts.Log.With("component", "pgstore"),
		subs:  newSubscribers(),
		ready: make(chan struct{}, 1),
	}, nil
}

// DB exposes the pool for the migration runner and for tests. Production code
// goes through the Store.
func (s *Store) DB() *sql.DB { return s.db }

// Close marks the store closed and, if it opened the pool, closes it.
//
// Like memstore's, this is a STATE CHANGE rather than a teardown: every
// subsequent call returns ErrClosed instead of a driver error or a panic, so a
// worker still settling an outcome during shutdown fails cleanly.
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.subs.closeAll()
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

// Ping reports whether the database is reachable. It backs GET /api/v1/ready.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	return s.db.PingContext(ctx)
}

// Ready is the dispatcher's wake-up hint. See the field comment on Store.ready.
func (s *Store) Ready() <-chan struct{} { return s.ready }

func (s *Store) signalReady() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// guard is the first line of every method: a cancelled context and a closed
// store are reported the same way here as in memstore, which is what lets one
// conformance suite cover both.
func (s *Store) guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return runmesh.ErrClosed
	}
	return nil
}

// ─── transactions ────────────────────────────────────────────────────────────

// inTx runs fn in a transaction, rolling back on any error or panic.
//
// The isolation level is the default READ COMMITTED, deliberately. Every
// mutation holds the job row for its whole duration, so the anomalies a higher
// level would prevent cannot occur — and SERIALIZABLE would add retryable
// serialisation failures on a workload that has no need of them.
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pgstore: beginning a transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful Commit is a documented no-op, so this
		// needs no "did we commit" flag and cannot mask the real error.
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pgstore: committing: %w", err)
	}
	return nil
}

// jobTx is one job, locked, loaded, and about to be written back.
//
// It records each step's version at load time so flush can write back exactly
// the rows the policy touched. Every mutation bumps a version, so "changed"
// needs no dirty flags threaded through the policy — which is what lets the
// policy stay pure and shared with memstore.
type jobTx struct {
	tx     *sql.Tx
	job    *runmesh.Job
	before map[string]uint64
	jobVer uint64
	events []runmesh.Event
}

// emit buffers an event. Sequence numbers are assigned in flush, inside this
// same transaction and under the same job lock, which is what keeps the per-job
// sequence gap-free: two appends cannot both read the same counter.
func (t *jobTx) emit(e runmesh.Event) { t.events = append(t.events, e) }

// reconcile applies the shared transition policy.
func (t *jobTx) reconcile(now time.Time) {
	jobstate.Reconcile(t.job, now, t.emit)
}

// lockJob takes the job row and loads the job with its steps.
//
// FOR UPDATE, not FOR NO KEY UPDATE: the job row is being used as a mutex, and
// the weaker mode would let two writers through.
func lockJob(ctx context.Context, tx *sql.Tx, id string) (*jobTx, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1 FOR UPDATE`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runmesh.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: loading job %s: %w", id, err)
	}
	if err := loadSteps(ctx, tx, job); err != nil {
		return nil, err
	}

	before := make(map[string]uint64, len(job.Steps))
	for _, st := range job.Steps {
		before[st.ID] = st.Version
	}
	return &jobTx{tx: tx, job: job, before: before, jobVer: job.Version}, nil
}

// loadSteps fills in a job's steps in PLAN order. Execution order comes from
// the DAG; this is only the order a submitted plan reads back in.
func loadSteps(ctx context.Context, tx *sql.Tx, job *runmesh.Job) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT `+stepColumns+` FROM job_steps WHERE job_id = $1 ORDER BY ordinal`, job.ID)
	if err != nil {
		return fmt.Errorf("pgstore: loading steps of %s: %w", job.ID, err)
	}
	defer func() { _ = rows.Close() }()

	job.Steps = nil
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return fmt.Errorf("pgstore: loading steps of %s: %w", job.ID, err)
		}
		job.Steps = append(job.Steps, st)
	}
	return rows.Err()
}

// flush writes back the job row, every step whose version changed, and every
// buffered event.
func (t *jobTx) flush(ctx context.Context) error {
	for _, st := range t.job.Steps {
		if t.before[st.ID] == st.Version {
			continue
		}
		if err := updateStep(ctx, t.tx, t.job.ID, st); err != nil {
			return err
		}
		t.before[st.ID] = st.Version
	}

	for i := range t.events {
		t.job.EventSeq++
		t.events[i].Seq = t.job.EventSeq
		t.events[i].JobID = t.job.ID
	}

	if t.job.Version != t.jobVer || len(t.events) > 0 {
		if err := updateJob(ctx, t.tx, t.job); err != nil {
			return err
		}
		t.jobVer = t.job.Version
	}

	if len(t.events) == 0 {
		return nil
	}
	written, err := insertEvents(ctx, t.tx, t.events)
	if err != nil {
		return err
	}
	// Publishing happens after the transaction commits, so a subscriber never
	// sees an event that a rollback then un-wrote. The caller collects these.
	t.events = written
	return nil
}

func updateJob(ctx context.Context, tx *sql.Tx, j *runmesh.Job) error {
	jobErr, err := encodeErrorInfo(j.Error)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE jobs SET
			state = $2, cancel_requested_at = $3, cancel_reason = $4, error = $5,
			updated_at = $6, started_at = $7, ended_at = $8,
			version = $9, event_seq = $10
		WHERE id = $1`,
		j.ID, j.State.String(), nullTime(j.CancelRequestedAt), string(j.CancelReason),
		jobErr, j.UpdatedAt, nullTime(j.StartedAt), nullTime(j.EndedAt),
		int64(j.Version), int64(j.EventSeq))
	if err != nil {
		return fmt.Errorf("pgstore: updating job %s: %w", j.ID, err)
	}
	return nil
}

func updateStep(ctx context.Context, tx *sql.Tx, jobID string, s *runmesh.Step) error {
	stepErr, err := encodeErrorInfo(s.Error)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE job_steps SET
			state = $3, attempt = $4, failures = $5, next_attempt_at = $6,
			lease_id = $7, lease_owner = $8, lease_expires_at = $9,
			result = $10, error = $11,
			scheduled_at = $12, started_at = $13, ended_at = $14, version = $15
		WHERE job_id = $1 AND id = $2`,
		jobID, s.ID, s.State.String(), int64(s.Attempt), int64(s.Failures), s.NextAttemptAt,
		nullString(s.LeaseID), nullString(s.LeaseOwner), nullZeroTime(s.LeaseExpiresAt),
		nullRaw(s.Result), stepErr,
		nullTime(s.ScheduledAt), nullTime(s.StartedAt), nullTime(s.EndedAt), int64(s.Version))
	if err != nil {
		return fmt.Errorf("pgstore: updating step %s/%s: %w", jobID, s.ID, err)
	}
	return nil
}

// ─── writes ──────────────────────────────────────────────────────────────────

// CreateJob persists a validated job, honouring an idempotency key.
//
// The replay check is guarded by a transaction-scoped advisory lock on the key
// rather than by catching a unique-violation error code. Both are correct; the
// lock keeps this file free of driver-specific SQLSTATE handling, and it turns
// a concurrent replay into a short wait rather than an aborted transaction that
// has to be retried from the top. The unique index is still there, as the
// constraint that makes the invariant true rather than merely observed.
func (s *Store) CreateJob(ctx context.Context, j *runmesh.Job, idemKey string) (*runmesh.Job, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if j == nil || j.ID == "" {
		return nil, fmt.Errorf("pgstore: CreateJob requires a job with an id")
	}

	stored := j.Clone()
	stored.IdempotencyKey = idemKey

	var replayed *runmesh.Job
	var events []runmesh.Event

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if idemKey != "" {
			if _, err := tx.ExecContext(ctx,
				`SELECT pg_advisory_xact_lock($1::int4, $2::int4)`,
				advisoryNamespace, int32(hash32(idemKey))); err != nil {
				return fmt.Errorf("pgstore: locking idempotency key: %w", err)
			}
			var existing string
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM jobs WHERE idempotency_key = $1`, idemKey).Scan(&existing)
			switch {
			case err == nil:
				replayed, err = loadJob(ctx, tx, existing)
				return err
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("pgstore: checking idempotency key: %w", err)
			}
		}

		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT true FROM jobs WHERE id = $1`, j.ID).Scan(&exists); err == nil {
			return runmesh.ErrConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("pgstore: checking job id: %w", err)
		}

		if err := insertJob(ctx, tx, stored); err != nil {
			return err
		}

		stored.EventSeq++
		created := runmesh.Event{
			JobID: stored.ID,
			Seq:   stored.EventSeq,
			Type:  runmesh.JobCreated,
			At:    stored.CreatedAt,
			State: stored.State,
			Attrs: map[string]any{"steps": len(stored.Steps), "name": stored.Name},
		}
		written, err := insertEvents(ctx, tx, []runmesh.Event{created})
		if err != nil {
			return err
		}
		events = written
		return updateJob(ctx, tx, stored)
	})
	if err != nil {
		return nil, err
	}
	if replayed != nil {
		return replayed, runmesh.ErrDuplicate
	}

	s.publish(events)
	s.signalReady()
	return stored.Clone(), nil
}

func insertJob(ctx context.Context, tx *sql.Tx, j *runmesh.Job) error {
	jobErr, err := encodeErrorInfo(j.Error)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO jobs (id, name, state, priority, on_step_failure, idempotency_key,
			cancel_requested_at, cancel_reason, error,
			created_at, updated_at, started_at, ended_at, version, event_seq)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		j.ID, j.Name, j.State.String(), int64(j.Priority), j.OnStepFailure.String(),
		nullString(j.IdempotencyKey), nullTime(j.CancelRequestedAt), string(j.CancelReason),
		jobErr, j.CreatedAt, j.UpdatedAt, nullTime(j.StartedAt), nullTime(j.EndedAt),
		int64(j.Version), int64(j.EventSeq))
	if err != nil {
		return fmt.Errorf("pgstore: inserting job %s: %w", j.ID, err)
	}

	for i, st := range j.Steps {
		stepErr, err := encodeErrorInfo(st.Error)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO job_steps (job_id, id, ordinal, tool, params, depends_on, state,
				timeout_ns, max_attempts, attempt, failures, next_attempt_at,
				lease_id, lease_owner, lease_expires_at, result, error,
				scheduled_at, started_at, ended_at, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			j.ID, st.ID, int64(i), st.Tool, nullRaw(st.Params), textArray(st.DependsOn),
			st.State.String(), int64(st.Timeout), int64(st.MaxAttempts),
			int64(st.Attempt), int64(st.Failures), st.NextAttemptAt,
			nullString(st.LeaseID), nullString(st.LeaseOwner), nullZeroTime(st.LeaseExpiresAt),
			nullRaw(st.Result), stepErr,
			nullTime(st.ScheduledAt), nullTime(st.StartedAt), nullTime(st.EndedAt),
			int64(st.Version))
		if err != nil {
			return fmt.Errorf("pgstore: inserting step %s/%s: %w", j.ID, st.ID, err)
		}
	}
	return nil
}

// RequestCancel raises the cancel flag and cancels every step that has not
// started.
//
// It deliberately does NOT touch SCHEDULED or RUNNING steps: a worker owns
// those, and they learn to stop through their next heartbeat. That is the only
// mechanism that works now that the cancelling API call may land on a different
// replica from the one executing the step — which, from this week, it can.
func (s *Store) RequestCancel(ctx context.Context, id string, reason runmesh.CancelReason, now time.Time) (*runmesh.Job, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	var out *runmesh.Job
	var events []runmesh.Event
	var conflict bool

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := lockJob(ctx, tx, id)
		if err != nil {
			return err
		}
		if t.job.State.Terminal() {
			out, conflict = t.job, true
			return nil
		}
		if t.job.CancelRequestedAt != nil {
			out = t.job // idempotent: cancelling twice is not an error
			return nil
		}

		at := now
		t.job.CancelRequestedAt = &at
		t.job.CancelReason = reason
		t.emit(runmesh.Event{
			Type:  runmesh.JobCancelRequested,
			At:    now,
			State: t.job.State,
			Attrs: map[string]any{"reason": string(reason)},
		})
		t.reconcile(now)
		if err := t.flush(ctx); err != nil {
			return err
		}
		out, events = t.job, t.events
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.publish(events)
	s.signalReady()
	if conflict {
		return out, runmesh.ErrConflict
	}
	return out, nil
}

// ─── reads ───────────────────────────────────────────────────────────────────

// Job returns one job with its steps.
func (s *Store) Job(ctx context.Context, id string) (*runmesh.Job, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runmesh.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: loading job %s: %w", id, err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+stepColumns+` FROM job_steps WHERE job_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return nil, fmt.Errorf("pgstore: loading steps of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: loading steps of %s: %w", id, err)
		}
		job.Steps = append(job.Steps, st)
	}
	return job, rows.Err()
}

// loadJob is Job's in-transaction twin, used where a lock is already held.
func loadJob(ctx context.Context, tx *sql.Tx, id string) (*runmesh.Job, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runmesh.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pgstore: loading job %s: %w", id, err)
	}
	if err := loadSteps(ctx, tx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// ListJobs returns a page of jobs, newest first.
//
// Ordering is by id descending, and that works because ids are k-sortable:
// keyset pagination needs no tie-breaker column and never uses OFFSET, so page
// 500 costs exactly what page 1 costs.
func (s *Store) ListJobs(ctx context.Context, f runmesh.JobFilter) (runmesh.JobPage, error) {
	if err := s.guard(ctx); err != nil {
		return runmesh.JobPage{}, err
	}
	limit := f.Limit
	if limit < 1 {
		limit = 50
	}

	states := make(textArray, 0, len(f.States))
	for _, st := range f.States {
		states = append(states, st.String())
	}

	// One row past the limit, so NextCursor is set only when a further page
	// genuinely exists rather than whenever a page happens to come out full.
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+jobColumns+` FROM jobs
		 WHERE ($1 = '' OR id < $1)
		   AND (cardinality($2::text[]) = 0 OR state = ANY($2::text[]))
		 ORDER BY id DESC
		 LIMIT $3`, f.Cursor, states, limit+1)
	if err != nil {
		return runmesh.JobPage{}, fmt.Errorf("pgstore: listing jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page runmesh.JobPage
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return runmesh.JobPage{}, fmt.Errorf("pgstore: listing jobs: %w", err)
		}
		if len(page.Jobs) == limit {
			page.NextCursor = page.Jobs[limit-1].ID
			break
		}
		page.Jobs = append(page.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return runmesh.JobPage{}, fmt.Errorf("pgstore: listing jobs: %w", err)
	}
	if err := loadStepsForPage(ctx, s.db, page.Jobs); err != nil {
		return runmesh.JobPage{}, err
	}
	return page, nil
}

// loadStepsForPage fills in the steps of every job on a page in ONE query.
//
// GET /api/v1/jobs renders each job with its steps, so a page without them
// would be a different response body from the same endpoint depending on which
// store was configured — the exact divergence the two implementations exist to
// avoid, and one the conformance suite now asserts against
// (ListJobsIncludesSteps).
//
// The obvious implementation is a query per job, which is the N+1 that makes
// list endpoints slow. One IN-list and a group-by in Go is a second round trip
// regardless of page size.
func loadStepsForPage(ctx context.Context, db *sql.DB, jobs []*runmesh.Job) error {
	if len(jobs) == 0 {
		return nil
	}
	ids := make(textArray, len(jobs))
	byID := make(map[string]*runmesh.Job, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
		byID[j.ID] = j
	}

	rows, err := db.QueryContext(ctx,
		`SELECT job_id, `+stepColumns+` FROM job_steps
		  WHERE job_id = ANY ($1::text[])
		  ORDER BY job_id, ordinal`, ids)
	if err != nil {
		return fmt.Errorf("pgstore: loading steps for a job page: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		// The job id is scanned off the front of the same column list the
		// single-job path uses, so the two cannot drift.
		var jobID string
		st, err := scanStepWithPrefix(rows, &jobID)
		if err != nil {
			return fmt.Errorf("pgstore: loading steps for a job page: %w", err)
		}
		if j := byID[jobID]; j != nil {
			j.Steps = append(j.Steps, st)
		}
	}
	return rows.Err()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// hash32 turns an idempotency key into the second half of an advisory lock id.
// Collisions are harmless: two unrelated keys sharing a lock serialise for a
// few microseconds, and the unique index remains the actual guarantee.
func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// publish fans out committed events to in-process subscribers.
//
// Committed is the operative word: it runs after the transaction, so a
// subscriber can never observe an event that a rollback then un-wrote. The
// price is that a crash between commit and publish loses the live notification;
// the durable timeline is unaffected, and a client resumes from its cursor.
func (s *Store) publish(events []runmesh.Event) {
	for _, e := range events {
		s.subs.publish(e)
	}
}

// subscribers is the in-process live event fan-out. Like memstore's, a slow
// consumer loses events rather than stalling execution: observability must
// never apply backpressure to work.
type subscribers struct {
	mu      sync.Mutex
	set     map[*subscriber]struct{}
	dropped atomic.Uint64
}

type subscriber struct {
	ch   chan runmesh.Event
	once sync.Once
}

func (s *subscriber) close() { s.once.Do(func() { close(s.ch) }) }

func newSubscribers() *subscribers {
	return &subscribers{set: make(map[*subscriber]struct{})}
}

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

func (s *subscribers) publish(e runmesh.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		select {
		case sub.ch <- e.Clone():
		default:
			s.dropped.Add(1)
		}
	}
}

func (s *subscribers) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		sub.close()
		delete(s.set, sub)
	}
}

// Subscribe returns a live event channel and the function that ends the
// subscription.
//
// It carries events written by THIS process only. A second replica's events
// reach a dashboard through the durable timeline, which is why the resume
// cursor exists and why a client always drains it before switching to the live
// feed. Cross-replica push is LISTEN/NOTIFY, in Week 6.
func (s *Store) Subscribe(buf int) (<-chan runmesh.Event, func()) { return s.subs.add(buf) }

// Dropped counts events no subscriber had buffer room for.
func (s *Store) Dropped() uint64 { return s.subs.dropped.Load() }
