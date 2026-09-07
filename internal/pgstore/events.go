package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// eventColumns keeps the SELECT and scanEvent from drifting apart.
const eventColumns = `global_seq, job_id, seq, step_id, attempt, type, at,
	state, error, duration_ms, attrs`

// insertEvents writes buffered events and returns them with their global
// cursors filled in.
//
// Seq is already assigned by the caller, inside the transaction that holds the
// job lock — which is what makes the per-job sequence gap-free. GlobalSeq comes
// from the sequence and is assigned here. The UNIQUE (job_id, seq) constraint
// turns a mistake in that protocol into a failed transaction rather than a
// timeline with a hole in it.
func insertEvents(ctx context.Context, tx *sql.Tx, events []runmesh.Event) ([]runmesh.Event, error) {
	out := make([]runmesh.Event, len(events))
	copy(out, events)

	for i := range out {
		e := &out[i]
		errInfo, err := encodeErrorInfo(e.Error)
		if err != nil {
			return nil, err
		}
		attrs, err := encodeAttrs(e.Attrs)
		if err != nil {
			return nil, err
		}

		var state any
		if e.State != runmesh.Unknown {
			state = e.State.String()
		}
		var globalSeq int64
		err = tx.QueryRowContext(ctx, `
			INSERT INTO job_events (job_id, seq, step_id, attempt, type, at,
				state, error, duration_ms, attrs)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			RETURNING global_seq`,
			e.JobID, int64(e.Seq), nullString(e.StepID), int64(e.Attempt),
			string(e.Type), e.At, state, errInfo, e.DurationMS, attrs).Scan(&globalSeq)
		if err != nil {
			return nil, fmt.Errorf("pgstore: appending event %s for %s: %w", e.Type, e.JobID, err)
		}
		e.GlobalSeq = uint64(globalSeq)
	}
	return out, nil
}

// JobEvents returns one page of a job's timeline, starting after afterSeq.
//
// Truncated is computed from the oldest surviving row rather than hard-coded to
// false. PostgreSQL never evicts, so today it is always false — but a retention
// policy that prunes old events is a natural thing to add, and when it lands
// this page will start telling clients the truth about the gap without anyone
// remembering to come back here.
func (s *Store) JobEvents(ctx context.Context, jobID string, afterSeq uint64, limit int) (runmesh.EventPage, error) {
	if err := s.guard(ctx); err != nil {
		return runmesh.EventPage{}, err
	}
	if limit < 1 {
		limit = 200
	}

	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT true FROM jobs WHERE id = $1`, jobID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return runmesh.EventPage{}, runmesh.ErrNotFound
	}
	if err != nil {
		return runmesh.EventPage{}, fmt.Errorf("pgstore: reading job %s: %w", jobID, err)
	}

	page := runmesh.EventPage{NextAfter: afterSeq}

	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT min(seq) FROM job_events WHERE job_id = $1`, jobID).Scan(&oldest); err != nil {
		return runmesh.EventPage{}, fmt.Errorf("pgstore: reading the timeline of %s: %w", jobID, err)
	}
	if !oldest.Valid {
		return page, nil
	}
	page.OldestSeq = uint64(oldest.Int64)
	// Written as a subtraction rather than afterSeq+1 < oldest: Seq is a uint64,
	// so an absurd cursor would wrap to zero and report a gap on a timeline that
	// has none.
	page.Truncated = page.OldestSeq > 1 && afterSeq < page.OldestSeq-1

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+eventColumns+` FROM job_events
		 WHERE job_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`, jobID, clampCursor(afterSeq), limit)
	if err != nil {
		return runmesh.EventPage{}, fmt.Errorf("pgstore: reading the timeline of %s: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return runmesh.EventPage{}, fmt.Errorf("pgstore: reading the timeline of %s: %w", jobID, err)
		}
		page.Events = append(page.Events, e)
		page.NextAfter = e.Seq
	}
	return page, rows.Err()
}

// TailEvents returns store-wide events after a global cursor. It is what lets a
// reconnecting dashboard catch up on everything it missed, across all jobs,
// before switching to the live subscription.
//
// See the note on job_events.global_seq in the migration: allocation order is
// not commit order, so this is a catch-up mechanism, not a substitute for the
// per-job cursor a client resumes from.
func (s *Store) TailEvents(ctx context.Context, afterGlobal uint64, limit int) ([]runmesh.Event, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+eventColumns+` FROM job_events
		 WHERE global_seq > $1
		 ORDER BY global_seq
		 LIMIT $2`, clampCursor(afterGlobal), limit)
	if err != nil {
		return nil, fmt.Errorf("pgstore: tailing events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []runmesh.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: tailing events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// clampCursor makes a uint64 cursor safe to bind to a BIGINT.
//
// Cursors come off the wire, so a client can send 2^64-1. Binding that to a
// signed column overflows and the driver rejects the query — the caller would
// see a 500 for what is really "you asked for events after the end of time".
// Clamping answers the question they actually asked: nothing follows.
func clampCursor(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
