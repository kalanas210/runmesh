package engine

import (
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Observer is the engine's telemetry seam, declared HERE by its consumer for
// the same reason Store and Sandbox are: internal/engine does not import
// internal/metrics, and a test satisfies this with a struct of ten empty
// methods.
//
// Three things about it are load-bearing, and none of them is enforceable by
// the type system, so they are written out instead.
//
// (1) EVERY METHOD IS CALLED ON AN ENGINE GOROUTINE. AttemptStarted,
// ToolExecuted, AttemptSettled and Heartbeat run on a worker that is holding
// one of the pool's capacity tokens; Claimed, Dispatched and DispatcherIdle run
// on the single dispatcher; SweepFinished and LeaseReclaimed run on the
// reconciler; ConcurrencyAdjusted runs on the adaptive controller, at most
// once per ConcurrencyInterval, which is the one method here with headroom to
// spare against this rule rather than needing every bit of it. An
// implementation that takes a lock, allocates per call, writes to an
// unsynchronised map or logs does not make metrics slow — it SHRINKS THE
// WORKER POOL, and does so silently, showing up as reduced throughput with no
// error anywhere. The shipped implementation is atomics plus one map read under
// a read lock, and TestObserveIsAllocationFree asserts the allocation half with
// testing.AllocsPerRun rather than leaving it as prose.
//
// (2) NO METHOD RETURNS ANYTHING. An observer cannot change an execution
// decision, cannot fail a step, and cannot make a claim retry. Observability
// that can alter behaviour is not observability; it is a second, undocumented
// policy layer. The compiler enforces this half.
//
// (3) THIS IS DELIBERATELY NOT THE EVENT STREAM. runmesh.Event is the durable,
// ordered, replayable product surface: it is appended inside the same critical
// section as the state change that produced it, it carries two cursors, and a
// dashboard can resume from either. This is a lossy in-process side channel for
// counters. Feeding metrics from Subscribe instead would couple the exporter to
// a fan-out whose delivery is a non-blocking send that DROPS when a subscriber
// is slow, and to a ring that EVICTS — and a counter fed by a lossy channel is
// a counter that lies, with a rate() that cannot be repaired after the fact.
// Conflating the two makes the durable thing carry a load it was not designed
// for and makes the lossy thing silently wrong.
type Observer interface {
	// Claimed reports one Store.Claim round trip. requested is the number of
	// capacity tokens the dispatcher was holding; returned is what the store
	// gave back, which is always <= requested by Claim's contract and is zero
	// whenever err is non-nil.
	Claimed(requested, returned int, d time.Duration, err error)

	// Dispatched reports a lease reaching a worker. queueWait is measured from
	// Lease.ClaimedAt, so it is the time a claimed step spent waiting for
	// capacity rather than the time it spent queued in the store.
	Dispatched(tool string, queueWait time.Duration)

	// DispatcherIdle reports what woke the dispatcher out of an idle park:
	// "hint" for Store.Ready(), "tick" for the poll interval, "shutdown" for a
	// cancelled context. The hint-to-tick ratio is what says whether the store's
	// readiness hint earns its keep against plain polling.
	DispatcherIdle(wokeOn string)

	// AttemptStarted reports a step moving from claimed to running.
	// dispatchWait is Store.Start's timestamp minus Lease.ClaimedAt — under
	// Kubernetes, that gap is pod pending time, which is exactly the span the
	// Store.Start doc says the execution waterfall needs as its own.
	AttemptStarted(tool string, dispatchWait time.Duration)

	// ToolExecuted brackets Executor.Execute and NOTHING else. The difference
	// between this and the attempt duration reported by AttemptSettled is the
	// store write plus the heartbeats, and that difference is what says whether
	// the engine or the tool got slower.
	ToolExecuted(tool string, d time.Duration)

	// AttemptSettled reports a decided outcome. It is called on EVERY exit path
	// of settle, including the ones that persist nothing, because the number
	// worth counting is what the worker decided — the same number the
	// "step settled" log line reports.
	AttemptSettled(o AttemptOutcome)

	// Heartbeat reports one heartbeat round trip. outcome is "ok",
	// "lease_lost", "cancel" or "error", which are the four distinguishable
	// answers the store's Directive and error can carry together.
	Heartbeat(tool, outcome string)

	// LeaseReclaimed reports one lease the reconciler took back. newState
	// distinguishes a step that will retry (QUEUED) from one whose retry budget
	// the expiry just spent (FAILED), which is the difference between a slow
	// worker and a crash loop.
	LeaseReclaimed(newState runmesh.State)

	// SweepFinished reports one reconciler sweep. reclaimed is zero when err is
	// non-nil.
	SweepFinished(reclaimed int, d time.Duration, err error)

	// ConcurrencyAdjusted reports one adaptive-sizing decision: the pool's
	// target moved from `from` workers to `to`. Called at most once per
	// Config.ConcurrencyInterval, from the controller goroutine runConcurrency
	// owns (resize.go), and only when the target actually changed — a tick
	// that leaves size where it was is not a decision, and counting it would
	// bury the rate a real move happens at under a rate a fixed pool would
	// share.
	ConcurrencyAdjusted(from, to int)
}

// AttemptOutcome is everything settle decided about one attempt, flattened into
// the shape a counter wants.
//
// It carries the Disposition's three booleans separately rather than a single
// "result" string because they are not mutually exclusive in principle and
// because a counter that collapses them would make "how many outcomes did we
// throw away because another worker had already taken the step" unanswerable —
// which is the first question asked after a lease-expiry incident.
type AttemptOutcome struct {
	Tool  string
	State runmesh.State
	Stop  Stop
	// Code is the classified error code, or "" when the attempt succeeded. A
	// tool may emit a code of its own, so this is the one label value in the
	// engine that is not drawn from a closed vocabulary.
	Code     string
	Duration time.Duration
	// Discarded: another worker owns this step, so nothing was written at all.
	Discarded bool
	// Released: the step went back to QUEUED without spending retry budget.
	Released bool
	Reason   string
}

// nopObserver is the default. It exists so that no call site has to nil-check,
// and so that not one of the engine's existing tests had to change when the
// seam arrived — the same shape policy.Passthrough set for Sandbox in Week 4.
type nopObserver struct{}

func (nopObserver) Claimed(int, int, time.Duration, error)  {}
func (nopObserver) Dispatched(string, time.Duration)        {}
func (nopObserver) DispatcherIdle(string)                   {}
func (nopObserver) AttemptStarted(string, time.Duration)    {}
func (nopObserver) ToolExecuted(string, time.Duration)      {}
func (nopObserver) AttemptSettled(AttemptOutcome)           {}
func (nopObserver) Heartbeat(string, string)                {}
func (nopObserver) LeaseReclaimed(runmesh.State)            {}
func (nopObserver) SweepFinished(int, time.Duration, error) {}
func (nopObserver) ConcurrencyAdjusted(int, int)            {}

var _ Observer = nopObserver{}
