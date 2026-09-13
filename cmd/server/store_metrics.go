package main

import (
	"context"
	"errors"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/metrics"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// observedStore times the store's calls and classifies their errors.
//
// A DECORATOR, AND IN THIS FILE, for two reasons that are both about what is
// NOT touched. The obvious alternative is a method on engine.Store so a store
// reports its own latency — but that interface's own doc treats "no edit to
// this file" as the proof the abstraction was designed, and adding a method
// there is a four-file change: memstore, pgstore, storetest.Store, and every
// double in every test. A decorator over the existing seven methods gets the
// same numbers with no edit to the contract at all.
//
// And cmd/server is the right place for it rather than a package of its own,
// because this is the ONLY file in the codebase that names both engine.Store
// and httpapi.Store — the union at run.go is right here — so a wrapper that has
// to satisfy both belongs where both are already spelled.
//
// Everything not overridden passes straight through the embedded interface:
// CreateJob, Job, ListJobs, RequestCancel, JobEvents, Ping, Ready, Dropped and
// Close are untouched, so adding a method to either interface later does not
// break this file.
type observedStore struct {
	store
	m   *metrics.Set
	clk clock.Clock
}

// Compile-time proof the decorator is still the union everything downstream
// expects. Without it, a missing method would surface as a confusing error at
// the engine.New call site rather than here.
var _ store = observedStore{}

// Store operation names. They are the label vocabulary
// runmesh_store_operation_duration_seconds declares, and they are spelled as
// constants so a typo is a compile error rather than a silent "other" bucket
// on a dashboard.
const (
	opClaim        = "claim"
	opStart        = "start"
	opHeartbeat    = "heartbeat"
	opFinish       = "finish"
	opRelease      = "release"
	opExpireLeases = "expire_leases"
	opQueueDepth   = "queue_depth"
)

func (s observedStore) Claim(ctx context.Context, req runmesh.ClaimRequest) ([]runmesh.Lease, error) {
	start := s.clk.Now()
	out, err := s.store.Claim(ctx, req)
	s.observe(opClaim, start, err)
	return out, err
}

func (s observedStore) Start(ctx context.Context, l runmesh.Lease, now time.Time) error {
	start := s.clk.Now()
	err := s.store.Start(ctx, l, now)
	s.observe(opStart, start, err)
	return err
}

func (s observedStore) Heartbeat(ctx context.Context, l runmesh.Lease, now, until time.Time) (runmesh.Directive, error) {
	start := s.clk.Now()
	dir, err := s.store.Heartbeat(ctx, l, now, until)
	s.observe(opHeartbeat, start, err)
	return dir, err
}

func (s observedStore) Finish(ctx context.Context, o runmesh.Outcome) error {
	start := s.clk.Now()
	err := s.store.Finish(ctx, o)
	s.observe(opFinish, start, err)
	return err
}

func (s observedStore) Release(ctx context.Context, l runmesh.Lease, reason string, now time.Time) error {
	start := s.clk.Now()
	err := s.store.Release(ctx, l, reason, now)
	s.observe(opRelease, start, err)
	return err
}

func (s observedStore) ExpireLeases(ctx context.Context, now time.Time, limit int) ([]runmesh.Expired, error) {
	start := s.clk.Now()
	out, err := s.store.ExpireLeases(ctx, now, limit)
	s.observe(opExpireLeases, start, err)
	return out, err
}

func (s observedStore) QueueDepth(ctx context.Context, now time.Time) (int, error) {
	start := s.clk.Now()
	n, err := s.store.QueueDepth(ctx, now)
	s.observe(opQueueDepth, start, err)
	return n, err
}

func (s observedStore) observe(op string, start time.Time, err error) {
	s.m.StoreOperation(op, s.clk.Since(start), errorKind(err))
}

// errorKind maps an error onto the closed `kind` vocabulary.
//
// It discriminates with errors.Is against the same five sentinels the worker
// branches on, rather than inventing a parallel taxonomy — reusing them is what
// makes "the worker discarded an outcome" and
// "store_operation_errors_total{op=finish,kind=lease_lost} went up" the same
// event rather than two things a reader has to correlate by eye. Anything
// unmatched is "other", which is deliberately NOT an error-free answer: a
// rising `other` rate is the signal that a new failure mode arrived and this
// switch has not caught up with it.
func errorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, runmesh.ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, runmesh.ErrConflict):
		return "conflict"
	case errors.Is(err, runmesh.ErrNotFound):
		return "not_found"
	case errors.Is(err, runmesh.ErrDuplicate):
		return "duplicate"
	case errors.Is(err, runmesh.ErrClosed):
		return "closed"
	default:
		return "other"
	}
}
