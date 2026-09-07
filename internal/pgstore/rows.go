package pgstore

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// This file is the whole of the mapping between a domain value and a column,
// and it is hand-written rather than delegated to the driver's type system on
// purpose: everything here goes through database/sql's driver.Valuer and
// sql.Scanner, so nothing in the store depends on pgx's own encoders. Swapping
// the driver is a one-line change in Open, not a rewrite of every query.
//
// It is also where the two representation mismatches live, both of them
// deliberate:
//
//   - Go's zero time.Time means "unset" for a lease or an end timestamp; SQL
//     spells that NULL. Round-tripping a year-1 timestamp through timestamptz
//     would technically work and would put a nonsense value in every row an
//     operator ever looks at.
//   - timestamptz stores MICROSECONDS. A Go time carries nanoseconds, so a
//     value written and read back is truncated. Nothing in RunMesh compares
//     timestamps for equality at sub-microsecond resolution — durations are
//     reported in milliseconds — but the truncation is real and is stated here
//     rather than discovered in a flaky test.

// ─── columns ─────────────────────────────────────────────────────────────────

// The column lists live here, once, so a SELECT and the Scan that consumes it
// cannot drift apart. Every query that reads a job or a step uses these.
const jobColumns = `id, name, state, priority, on_step_failure, idempotency_key,
	cancel_requested_at, cancel_reason, error, created_at, updated_at,
	started_at, ended_at, version, event_seq`

const stepColumns = `id, tool, params, depends_on, state, timeout_ns, max_attempts,
	attempt, failures, next_attempt_at, lease_id, lease_owner, lease_expires_at,
	result, error, scheduled_at, started_at, ended_at, version`

// scanner is the one method *sql.Row and *sql.Rows have in common, so the same
// scan helper serves a single-row lookup and a loop.
type scanner interface{ Scan(dest ...any) error }

func scanJob(sc scanner) (*runmesh.Job, error) {
	var (
		j          runmesh.Job
		state      string
		policy     string
		idemKey    sql.NullString
		cancelAt   sql.NullTime
		cancelWhy  string
		jobErr     []byte
		startedAt  sql.NullTime
		endedAt    sql.NullTime
		version    int64
		eventSeq   int64
		createdAt  time.Time
		updatedAt  time.Time
		priorityIn int64
	)
	err := sc.Scan(&j.ID, &j.Name, &state, &priorityIn, &policy, &idemKey,
		&cancelAt, &cancelWhy, &jobErr, &createdAt, &updatedAt,
		&startedAt, &endedAt, &version, &eventSeq)
	if err != nil {
		return nil, err
	}

	if j.State, err = runmesh.ParseState(state); err != nil {
		return nil, err
	}
	if err := j.OnStepFailure.UnmarshalText([]byte(policy)); err != nil {
		return nil, err
	}
	j.Priority = int(priorityIn)
	j.IdempotencyKey = idemKey.String
	j.CancelRequestedAt = timePtr(cancelAt)
	j.CancelReason = runmesh.CancelReason(cancelWhy)
	if j.Error, err = decodeErrorInfo(jobErr); err != nil {
		return nil, err
	}
	j.CreatedAt = createdAt.UTC()
	j.UpdatedAt = updatedAt.UTC()
	j.StartedAt = timePtr(startedAt)
	j.EndedAt = timePtr(endedAt)
	j.Version = uint64(version)
	j.EventSeq = uint64(eventSeq)
	return &j, nil
}

// scanStepWithPrefix scans a step row preceded by extra leading columns, so a
// multi-job query can carry the job id without a second column list to keep in
// sync with stepColumns.
func scanStepWithPrefix(rows *sql.Rows, prefix ...any) (*runmesh.Step, error) {
	return scanStep(prefixScanner{rows: rows, prefix: prefix})
}

type prefixScanner struct {
	rows   *sql.Rows
	prefix []any
}

func (p prefixScanner) Scan(dest ...any) error {
	all := make([]any, 0, len(p.prefix)+len(dest))
	all = append(all, p.prefix...)
	all = append(all, dest...)
	return p.rows.Scan(all...)
}

func scanStep(sc scanner) (*runmesh.Step, error) {
	var (
		s          runmesh.Step
		params     []byte
		deps       textArray
		state      string
		timeoutNS  int64
		nextAt     time.Time
		leaseID    sql.NullString
		leaseOwner sql.NullString
		leaseUntil sql.NullTime
		result     []byte
		stepErr    []byte
		schedAt    sql.NullTime
		startedAt  sql.NullTime
		endedAt    sql.NullTime
		maxAtt     int64
		attempt    int64
		failures   int64
		version    int64
	)
	err := sc.Scan(&s.ID, &s.Tool, &params, &deps, &state, &timeoutNS, &maxAtt,
		&attempt, &failures, &nextAt, &leaseID, &leaseOwner, &leaseUntil,
		&result, &stepErr, &schedAt, &startedAt, &endedAt, &version)
	if err != nil {
		return nil, err
	}

	if s.State, err = runmesh.ParseState(state); err != nil {
		return nil, err
	}
	s.Params = rawOrNil(params)
	s.DependsOn = deps.slice()
	s.Timeout = time.Duration(timeoutNS)
	s.MaxAttempts = int(maxAtt)
	s.Attempt = int(attempt)
	s.Failures = int(failures)
	s.NextAttemptAt = nextAt.UTC()
	s.LeaseID = leaseID.String
	s.LeaseOwner = leaseOwner.String
	if leaseUntil.Valid {
		s.LeaseExpiresAt = leaseUntil.Time.UTC()
	}
	s.Result = rawOrNil(result)
	if s.Error, err = decodeErrorInfo(stepErr); err != nil {
		return nil, err
	}
	s.ScheduledAt = timePtr(schedAt)
	s.StartedAt = timePtr(startedAt)
	s.EndedAt = timePtr(endedAt)
	s.Version = uint64(version)
	return &s, nil
}

func scanEvent(sc scanner) (runmesh.Event, error) {
	var (
		e         runmesh.Event
		globalSeq int64
		seq       int64
		stepID    sql.NullString
		attempt   sql.NullInt64
		typ       string
		at        time.Time
		state     sql.NullString
		evErr     []byte
		duration  sql.NullInt64
		attrs     []byte
	)
	err := sc.Scan(&globalSeq, &e.JobID, &seq, &stepID, &attempt, &typ, &at,
		&state, &evErr, &duration, &attrs)
	if err != nil {
		return runmesh.Event{}, err
	}

	e.GlobalSeq = uint64(globalSeq)
	e.Seq = uint64(seq)
	e.StepID = stepID.String
	e.Attempt = int(attempt.Int64)
	e.Type = runmesh.EventType(typ)
	e.At = at.UTC()
	if state.Valid && state.String != "" {
		if e.State, err = runmesh.ParseState(state.String); err != nil {
			return runmesh.Event{}, err
		}
	}
	if e.Error, err = decodeErrorInfo(evErr); err != nil {
		return runmesh.Event{}, err
	}
	e.DurationMS = duration.Int64
	if len(attrs) > 0 {
		if err := json.Unmarshal(attrs, &e.Attrs); err != nil {
			return runmesh.Event{}, fmt.Errorf("pgstore: decoding event attrs: %w", err)
		}
	}
	return e, nil
}

// ─── value helpers ───────────────────────────────────────────────────────────

// nullTime maps Go's "unset" to SQL NULL for an optional timestamp.
func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// nullZeroTime does the same for a non-pointer field whose zero value means
// unset — LeaseExpiresAt, which is a time.Time rather than a *time.Time
// because a lease is cleared far more often than it is absent.
func nullZeroTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// rawOrNil preserves the difference between "no JSON at all" and "the JSON
// value null". The domain uses a nil json.RawMessage for absent, and the tests
// pin the distinction, so a zero-length slice from the driver must come back
// as nil rather than as an empty non-nil slice.
func rawOrNil(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	out := make(json.RawMessage, len(b))
	copy(out, b)
	return out
}

// nullRaw writes absent JSON as NULL rather than as the four bytes "null", so
// `WHERE result IS NULL` means what it looks like it means.
func nullRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func encodeErrorInfo(e *runmesh.ErrorInfo) (any, error) {
	if e == nil {
		return nil, nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("pgstore: encoding error info: %w", err)
	}
	return b, nil
}

func decodeErrorInfo(b []byte) (*runmesh.ErrorInfo, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var info runmesh.ErrorInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("pgstore: decoding error info: %w", err)
	}
	return &info, nil
}

func encodeAttrs(m map[string]any) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("pgstore: encoding event attrs: %w", err)
	}
	return b, nil
}

// ─── text[] ──────────────────────────────────────────────────────────────────

// textArray is a driver-independent codec for PostgreSQL's text[] literal.
//
// pgx would encode a []string for us. Writing the twenty lines instead keeps
// depends_on — the one array column in the schema — free of any driver's type
// system, which is what makes "swap the driver" a one-line change rather than
// an audit of every query.
type textArray []string

// slice returns the domain's representation: an empty array reads back as nil,
// matching Plan.Build, which leaves DependsOn nil for a step without
// dependencies. Preserving that keeps `depends_on` absent from the JSON rather
// than present as [].
func (a textArray) slice() []string {
	if len(a) == 0 {
		return nil
	}
	return a
}

// Value renders the PostgreSQL array literal. Every element is quoted, so a
// step id containing a comma, a brace or a backslash cannot change the shape
// of the literal — the injection-shaped bug this format invites.
func (a textArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range s {
			if r == '"' || r == '\\' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan parses the literal back. It accepts both the quoted and the bare
// element forms, because PostgreSQL only quotes elements that need it.
func (a *textArray) Scan(src any) error {
	var raw string
	switch v := src.(type) {
	case nil:
		*a = nil
		return nil
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("pgstore: cannot scan %T into a text[]", src)
	}

	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") || !strings.HasSuffix(raw, "}") {
		return fmt.Errorf("pgstore: %q is not a PostgreSQL array literal", raw)
	}
	body := raw[1 : len(raw)-1]
	if body == "" {
		*a = nil
		return nil
	}

	out := make([]string, 0, 4)
	var cur strings.Builder
	inQuotes, escaped := false, false
	for _, r := range body {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuotes = !inQuotes
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	*a = out
	return nil
}
