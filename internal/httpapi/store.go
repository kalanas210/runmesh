package httpapi

import (
	"context"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Store is the API's view of persistence, and it is deliberately a DIFFERENT,
// narrower interface from engine.Store over the same concrete store.
//
// The API cannot claim, lease, heartbeat, finish or expire anything. That is
// not a convention or a review rule — an HTTP handler that tried would not
// compile. One *memstore.Store satisfies both interfaces; neither package
// knows the other exists.
type Store interface {
	// CreateJob honours idemKey. On a replay it returns the EXISTING job
	// together with runmesh.ErrDuplicate, which the handler turns into a 200
	// rather than a second job — so a client retrying a submission after a
	// network timeout does not double-submit.
	CreateJob(ctx context.Context, j *runmesh.Job, idemKey string) (*runmesh.Job, error)

	Job(ctx context.Context, id string) (*runmesh.Job, error)
	ListJobs(ctx context.Context, f runmesh.JobFilter) (runmesh.JobPage, error)

	// RequestCancel raises the cancel flag, cancels every step that has not
	// started, and leaves SCHEDULED/RUNNING steps alone — they learn through
	// their next heartbeat. The job therefore stays RUNNING until they drain,
	// and the response says so: honest state beats fast state, and it is the
	// only answer that stays true when the cancel lands on a different replica.
	RequestCancel(ctx context.Context, id string, reason runmesh.CancelReason, now time.Time) (*runmesh.Job, error)

	JobEvents(ctx context.Context, jobID string, afterSeq uint64, limit int) (runmesh.EventPage, error)

	// QueueDepth powers admission control and, in Week 3, the queue_depth
	// gauge. It is the same predicate Claim uses, counted instead of taken.
	QueueDepth(ctx context.Context, now time.Time) (int, error)

	Ping(ctx context.Context) error
}
