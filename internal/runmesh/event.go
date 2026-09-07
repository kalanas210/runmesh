package runmesh

import "time"

// EventType is the vocabulary of the execution timeline. It is
// fixed now — including the constants nothing produces yet — so the dashboard's
// timeline renderer can be written against a stable set and Week 3/4 only add
// producers, never new concepts.
type EventType string

const (
	JobCreated         EventType = "JOB_CREATED"
	JobStarted         EventType = "JOB_STARTED"
	JobCancelRequested EventType = "JOB_CANCEL_REQUESTED"
	JobFinished        EventType = "JOB_FINISHED"

	StepScheduled      EventType = "STEP_SCHEDULED"
	StepStarted        EventType = "STEP_STARTED"
	StepFinished       EventType = "STEP_FINISHED"
	StepRetryScheduled EventType = "STEP_RETRY_SCHEDULED"
	StepReleased       EventType = "STEP_RELEASED" // requeued; retry budget untouched
	StepLeaseExpired   EventType = "STEP_LEASE_EXPIRED"

	// Reserved. No producer in Week 1.
	PodCreated      EventType = "POD_CREATED"       // Week 3
	PodDeleted      EventType = "POD_DELETED"       // Week 3
	ToolCalled      EventType = "TOOL_CALLED"       // Week 4
	StepOutputChunk EventType = "STEP_OUTPUT_CHUNK" // Week 4
)

// Event is append-only.
//
// Seq is per-job, 1-based and gap-free: the cursor for GET /jobs/{id}/events
// and the Week-6 WebSocket resume token. GlobalSeq is store-wide and strictly
// increasing: the cursor for a dashboard-wide live feed. A reconnecting client
// drains TailEvents(lastGlobalSeq) and then switches to the live subscription,
// with no gaps and no duplicates — which is the whole reason there are two
// cursors rather than one.
type Event struct {
	GlobalSeq  uint64         `json:"global_seq"`
	Seq        uint64         `json:"seq"`
	JobID      string         `json:"job_id"`
	StepID     string         `json:"step_id,omitempty"`
	Attempt    int            `json:"attempt,omitempty"`
	Type       EventType      `json:"type"`
	At         time.Time      `json:"at"`
	State      State          `json:"state,omitempty"`
	Error      *ErrorInfo     `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// Clone deep-copies an event so a subscriber cannot mutate store-owned memory.
func (e Event) Clone() Event {
	c := e
	c.Error = e.Error.Clone()
	if e.Attrs != nil {
		c.Attrs = make(map[string]any, len(e.Attrs))
		for k, v := range e.Attrs {
			c.Attrs[k] = v
		}
	}
	return c
}

// EventPage is one page of a job's timeline.
type EventPage struct {
	Events    []Event `json:"events"`
	NextAfter uint64  `json:"next_after"`
	// Truncated reports that the requested cursor predates the oldest retained
	// event. The Week-1 in-memory ring evicts; PostgreSQL will not. Saying so
	// beats pretending the gap is not there.
	Truncated bool   `json:"truncated"`
	OldestSeq uint64 `json:"oldest_seq"`
}
