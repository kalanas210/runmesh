package httpapi

import (
	"encoding/json"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

// The wire types live here, separate from the domain types, and the domain is
// never marshalled to the wire directly.
//
// That costs a mapping function and buys two things: internal fields (lease
// tokens, idempotency keys, event sequence counters) cannot leak by accident
// when a field is added, and the wire format can evolve — durations as
// seconds, derived fields like blocked_by — without deforming the domain to
// suit JSON.

type jobResponse struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	State             runmesh.State  `json:"state"`
	Priority          int            `json:"priority"`
	OnStepFailure     string         `json:"on_step_failure"`
	Version           uint64         `json:"version"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	StartedAt         *time.Time     `json:"started_at"`
	EndedAt           *time.Time     `json:"ended_at"`
	DurationMS        *int64         `json:"duration_ms"`
	CancelRequestedAt *time.Time     `json:"cancel_requested_at"`
	CancelReason      string         `json:"cancel_reason,omitempty"`
	Error             *errorInfo     `json:"error"`
	Steps             []stepResponse `json:"steps"`
}

type stepResponse struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	DependsOn []string `json:"depends_on"`
	// BlockedBy is DERIVED on read: the dependencies that are not yet
	// SUCCEEDED. Nothing stores it, so nothing can disagree with the claim
	// predicate — and it makes the execution waterfall trivial to render.
	BlockedBy []string `json:"blocked_by"`

	State       runmesh.State `json:"state"`
	Attempt     int           `json:"attempt"`
	Failures    int           `json:"failures"`
	MaxAttempts int           `json:"max_attempts"`
	TimeoutSec  float64       `json:"timeout_seconds"`

	ScheduledAt   *time.Time `json:"scheduled_at"`
	StartedAt     *time.Time `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at"`
	NextAttemptAt *time.Time `json:"next_attempt_at"`
	DurationMS    *int64     `json:"duration_ms"`

	Result json.RawMessage `json:"result"`
	Error  *errorInfo      `json:"error"`

	Version uint64 `json:"version"`
}

type errorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Attempt   int    `json:"attempt"`
}

type jobListResponse struct {
	Jobs       []jobResponse `json:"jobs"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type eventsResponse struct {
	Events    []runmesh.Event `json:"events"`
	NextAfter uint64          `json:"next_after"`
	Truncated bool            `json:"truncated"`
	OldestSeq uint64          `json:"oldest_seq"`
}

type toolsResponse struct {
	Tools []tools.Descriptor `json:"tools"`
}

type healthResponse struct {
	Status string `json:"status"`
}

type readyResponse struct {
	Status string `json:"status"`
	Store  string `json:"store"`
	// Durable is the honest bit. Week 1 answers 201 to a submission as though
	// the job were safe, and it is not: a restart loses every job. The README
	// says so and so does this field, because a status endpoint that overstates
	// durability is worse than no status endpoint.
	Durable    bool   `json:"durable"`
	Workers    int    `json:"workers"`
	Inflight   int    `json:"inflight"`
	QueueDepth int    `json:"queue_depth"`
	Reason     string `json:"reason,omitempty"`
}

func toErrorInfo(e *runmesh.ErrorInfo) *errorInfo {
	if e == nil {
		return nil
	}
	return &errorInfo{Code: e.Code, Message: e.Message, Retryable: e.Retryable, Attempt: e.Attempt}
}

func toJobResponse(j *runmesh.Job) jobResponse {
	out := jobResponse{
		ID:                j.ID,
		Name:              j.Name,
		State:             j.State,
		Priority:          j.Priority,
		OnStepFailure:     j.OnStepFailure.String(),
		Version:           j.Version,
		CreatedAt:         j.CreatedAt,
		UpdatedAt:         j.UpdatedAt,
		StartedAt:         j.StartedAt,
		EndedAt:           j.EndedAt,
		DurationMS:        durationMS(j.StartedAt, j.EndedAt),
		CancelRequestedAt: j.CancelRequestedAt,
		CancelReason:      string(j.CancelReason),
		Error:             toErrorInfo(j.Error),
		Steps:             make([]stepResponse, 0, len(j.Steps)),
	}
	for _, s := range j.Steps {
		out.Steps = append(out.Steps, toStepResponse(j, s))
	}
	return out
}

func toStepResponse(j *runmesh.Job, s *runmesh.Step) stepResponse {
	r := stepResponse{
		ID:          s.ID,
		Tool:        s.Tool,
		DependsOn:   orEmpty(s.DependsOn),
		BlockedBy:   orEmpty(j.BlockedBy(s)),
		State:       s.State,
		Attempt:     s.Attempt,
		Failures:    s.Failures,
		MaxAttempts: s.MaxAttempts,
		TimeoutSec:  s.Timeout.Seconds(),
		ScheduledAt: s.ScheduledAt,
		StartedAt:   s.StartedAt,
		EndedAt:     s.EndedAt,
		DurationMS:  durationMS(s.StartedAt, s.EndedAt),
		Result:      s.Result,
		Error:       toErrorInfo(s.Error),
		Version:     s.Version,
	}
	// next_attempt_at is meaningful only while a step is waiting out a backoff.
	// Exposing it at other times would invite a dashboard to render a countdown
	// for a step that is not waiting for anything.
	if s.State == runmesh.Retrying && !s.NextAttemptAt.IsZero() {
		at := s.NextAttemptAt
		r.NextAttemptAt = &at
	}
	return r
}

// orEmpty turns a nil slice into an empty one, so the JSON says [] rather than
// null. A client iterating a list should not have to nil-check it.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func durationMS(from, to *time.Time) *int64 {
	if from == nil || to == nil {
		return nil
	}
	ms := to.Sub(*from).Milliseconds()
	return &ms
}
