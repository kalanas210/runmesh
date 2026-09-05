// Package storetest is the EXECUTABLE contract for a RunMesh store.
//
// internal/memstore passes it in Week 1. internal/pgstore must pass this exact
// file, unmodified, against a Testcontainers PostgreSQL in Week 2. Every other
// design in this project argues that its store interface is PostgreSQL-ready;
// this one makes the claim run, so a divergence between the in-memory
// simulation and real SKIP LOCKED semantics fails a test instead of surfacing
// in production three weeks later.
//
// Every case drives time through explicit `now` parameters, so the suite needs
// no clock, no goroutine and no sleep — which is also why it can run against a
// database with fabricated timestamps.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/httpapi"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Store is the full persistence surface: the union of what the engine needs
// and what the API needs, plus the two event methods that exist in Week 1 but
// are not consumed by production code until the Week-2 WebSocket handler.
type Store interface {
	engine.Store
	httpapi.Store
	TailEvents(ctx context.Context, afterGlobal uint64, limit int) ([]runmesh.Event, error)
	Subscribe(buf int) (<-chan runmesh.Event, func())
	Close() error
}

// Factory builds a fresh, empty store for one subtest.
type Factory func(t *testing.T) Store

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

const leaseTTL = 30 * time.Second

// RunSuite runs every conformance case against the given implementation.
func RunSuite(t *testing.T, newStore Factory) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Store)
	}{
		{"CreateAndRead", testCreateAndRead},
		{"IdempotentCreate", testIdempotentCreate},
		{"UnknownJob", testUnknownJob},
		{"CopyIsolation", testCopyIsolation},
		{"ClaimOrdering", testClaimOrdering},
		{"ClaimRespectsLimit", testClaimRespectsLimit},
		{"ClaimIsExclusive", testClaimIsExclusive},
		{"DependencyGating", testDependencyGating},
		{"FailedDependencyNeverUnblocks", testFailedDependencyNeverUnblocks},
		{"BackoffGate", testBackoffGate},
		{"FencingRejectsStaleLease", testFencingRejectsStaleLease},
		{"ReleaseRefundsBudgetExpireDoesNot", testReleaseRefundsBudgetExpireDoesNot},
		{"ExpireFailsWhenBudgetExhausted", testExpireFailsWhenBudgetExhausted},
		{"IllegalTransitions", testIllegalTransitions},
		{"HeartbeatDeliversCancel", testHeartbeatDeliversCancel},
		{"CancelLeavesLeasedStepsAlone", testCancelLeavesLeasedStepsAlone},
		{"CancelBlocksClaiming", testCancelBlocksClaiming},
		{"FailFastCancelsSiblings", testFailFastCancelsSiblings},
		{"FailFastAppliesToLeaseExpiryToo", testFailFastAppliesToLeaseExpiryToo},
		{"ReleaseOnACancelledJobDoesNotStrand", testReleaseOnACancelledJobDoesNotStrand},
		{"SubscribersAreIsolated", testSubscribersAreIsolated},
		{"AbsurdEventCursorDoesNotReportAFalseGap", testAbsurdEventCursorDoesNotReportAFalseGap},
		{"ContinueOnFailureDoomsDependents", testContinueOnFailureDoomsDependents},
		{"RollupPrecedence", testRollupPrecedence},
		{"EventSequences", testEventSequences},
		{"EventPaging", testEventPaging},
		{"TailEvents", testTailEvents},
		{"SubscribeDropsRatherThanBlocks", testSubscribeDropsRatherThanBlocks},
		{"SubscribeUnsubscribeIsRaceFree", testSubscribeUnsubscribeIsRaceFree},
		{"ReadyHintIsLossy", testReadyHintIsLossy},
		{"QueueDepthMatchesClaimPredicate", testQueueDepthMatchesClaimPredicate},
		{"ListJobsPaging", testListJobsPaging},
		{"ClosedStoreRejectsEverything", testClosedStoreRejectsEverything},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, newStore(t))
		})
	}
}

// ---------------------------------------------------------------- fixtures

// plan builds a job with the given steps. deps maps a step id to what it
// depends on.
func job(t *testing.T, id string, at time.Time, deps map[string][]string, order ...string) *runmesh.Job {
	t.Helper()
	p := &runmesh.Plan{Name: id, Steps: make([]runmesh.PlanStep, 0, len(order))}
	for _, s := range order {
		p.Steps = append(p.Steps, runmesh.PlanStep{ID: s, Tool: "echo", DependsOn: deps[s]})
	}
	return p.Build(id, at, runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3})
}

func mustCreate(t *testing.T, s Store, j *runmesh.Job) *runmesh.Job {
	t.Helper()
	out, err := s.CreateJob(t.Context(), j, "")
	if err != nil {
		t.Fatalf("CreateJob(%s): %v", j.ID, err)
	}
	return out
}

func mustClaim(t *testing.T, s Store, now time.Time, limit int) []runmesh.Lease {
	t.Helper()
	leases, err := s.Claim(t.Context(), runmesh.ClaimRequest{
		Owner: "test", Limit: limit, LeaseTTL: leaseTTL, Now: now,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(leases) > limit {
		t.Fatalf("Claim returned %d leases for limit %d", len(leases), limit)
	}
	return leases
}

// succeed drives one step all the way through a successful attempt.
func succeed(t *testing.T, s Store, l runmesh.Lease, now time.Time) {
	t.Helper()
	if err := s.Start(t.Context(), l, now); err != nil {
		t.Fatalf("Start(%s): %v", l.AttemptID, err)
	}
	err := s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Succeeded,
		Result:    json.RawMessage(`{"ok":true}`),
		StartedAt: now, EndedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("Finish(%s): %v", l.AttemptID, err)
	}
}

func failStep(t *testing.T, s Store, l runmesh.Lease, now time.Time, state runmesh.State) {
	t.Helper()
	if err := s.Start(t.Context(), l, now); err != nil {
		t.Fatalf("Start(%s): %v", l.AttemptID, err)
	}
	err := s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: state, CountFail: true,
		Error:     &runmesh.ErrorInfo{Code: "boom", Message: "injected", Attempt: l.Attempt},
		StartedAt: now, EndedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("Finish(%s): %v", l.AttemptID, err)
	}
}

func stepState(t *testing.T, s Store, jobID, stepID string) runmesh.State {
	t.Helper()
	j, err := s.Job(t.Context(), jobID)
	if err != nil {
		t.Fatalf("Job(%s): %v", jobID, err)
	}
	step := j.Step(stepID)
	if step == nil {
		t.Fatalf("job %s has no step %s", jobID, stepID)
	}
	return step.State
}

func jobState(t *testing.T, s Store, jobID string) runmesh.State {
	t.Helper()
	j, err := s.Job(t.Context(), jobID)
	if err != nil {
		t.Fatalf("Job(%s): %v", jobID, err)
	}
	return j.State
}

func eventTypes(t *testing.T, s Store, jobID string) []runmesh.EventType {
	t.Helper()
	page, err := s.JobEvents(t.Context(), jobID, 0, 1000)
	if err != nil {
		t.Fatalf("JobEvents(%s): %v", jobID, err)
	}
	out := make([]runmesh.EventType, 0, len(page.Events))
	for _, e := range page.Events {
		out = append(out, e.Type)
	}
	return out
}

// ------------------------------------------------------------------- cases

func testCreateAndRead(t *testing.T, s Store) {
	j := mustCreate(t, s, job(t, "job_a", epoch, nil, "one"))
	if j.State != runmesh.Queued {
		t.Fatalf("new job state = %s, want QUEUED", j.State)
	}
	got, err := s.Job(t.Context(), "job_a")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	if got.Name != "job_a" || len(got.Steps) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if types := eventTypes(t, s, "job_a"); len(types) != 1 || types[0] != runmesh.JobCreated {
		t.Fatalf("events = %v, want [JOB_CREATED]", types)
	}
}

func testIdempotentCreate(t *testing.T, s Store) {
	first, err := s.CreateJob(t.Context(), job(t, "job_a", epoch, nil, "one"), "key-1")
	if err != nil {
		t.Fatalf("first CreateJob: %v", err)
	}
	second, err := s.CreateJob(t.Context(), job(t, "job_b", epoch, nil, "one"), "key-1")
	if !errors.Is(err, runmesh.ErrDuplicate) {
		t.Fatalf("replay error = %v, want ErrDuplicate", err)
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("replay returned %v, want the original job %s", second, first.ID)
	}
	page, err := s.ListJobs(t.Context(), runmesh.JobFilter{})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(page.Jobs) != 1 {
		t.Fatalf("store holds %d jobs, want exactly 1", len(page.Jobs))
	}
}

func testUnknownJob(t *testing.T, s Store) {
	if _, err := s.Job(t.Context(), "job_missing"); !errors.Is(err, runmesh.ErrNotFound) {
		t.Fatalf("Job(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.JobEvents(t.Context(), "job_missing", 0, 10); !errors.Is(err, runmesh.ErrNotFound) {
		t.Fatalf("JobEvents(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := s.RequestCancel(t.Context(), "job_missing", runmesh.CancelUser, epoch); !errors.Is(err, runmesh.ErrNotFound) {
		t.Fatalf("RequestCancel(unknown) = %v, want ErrNotFound", err)
	}
	stale := runmesh.Lease{ID: "lse_x", JobID: "job_missing", StepID: "one"}
	if err := s.Start(t.Context(), stale, epoch); !errors.Is(err, runmesh.ErrNotFound) {
		t.Fatalf("Start(unknown) = %v, want ErrNotFound", err)
	}
}

// testCopyIsolation catches the aliasing bug a PostgreSQL store could never
// have: a caller holding a pointer into store-internal memory.
func testCopyIsolation(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, map[string][]string{"b": {"a"}}, "a", "b"))

	got, err := s.Job(t.Context(), "job_a")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	got.Name = "mutated"
	got.State = runmesh.Failed
	got.Steps[0].State = runmesh.Succeeded
	got.Steps[0].Tool = "mutated"
	got.Steps[1].DependsOn[0] = "mutated"

	again, err := s.Job(t.Context(), "job_a")
	if err != nil {
		t.Fatalf("Job: %v", err)
	}
	switch {
	case again.Name != "job_a":
		t.Error("mutating a returned job changed the stored name")
	case again.State != runmesh.Queued:
		t.Error("mutating a returned job changed the stored state")
	case again.Steps[0].State != runmesh.Queued:
		t.Error("mutating a returned step changed the stored step state")
	case again.Steps[0].Tool != "echo":
		t.Error("mutating a returned step changed the stored tool")
	case again.Steps[1].DependsOn[0] != "a":
		t.Error("mutating a returned depends_on slice reached store-internal memory")
	}
}

func testClaimOrdering(t *testing.T, s Store) {
	// Lower priority, created first.
	low := job(t, "job_low", epoch, nil, "b", "a")
	low.Priority = 1
	mustCreate(t, s, low)

	// Higher priority, created later: it must still win.
	high := job(t, "job_high", epoch.Add(time.Minute), nil, "z")
	high.Priority = 9
	mustCreate(t, s, high)

	leases := mustClaim(t, s, epoch.Add(2*time.Minute), 3)
	if len(leases) != 3 {
		t.Fatalf("claimed %d steps, want 3", len(leases))
	}
	want := []string{"job_high/z", "job_low/a", "job_low/b"}
	for i, l := range leases {
		if got := l.JobID + "/" + l.StepID; got != want[i] {
			t.Fatalf("claim order[%d] = %s, want %s (priority DESC, created_at, step id)", i, got, want[i])
		}
	}
	for _, l := range leases {
		if l.Attempt != 1 {
			t.Errorf("%s: first claim set attempt = %d, want 1", l.AttemptID, l.Attempt)
		}
		if l.ID == "" {
			t.Errorf("%s: claim did not stamp a fencing token", l.AttemptID)
		}
		if !l.ExpiresAt.Equal(epoch.Add(2 * time.Minute).Add(leaseTTL)) {
			t.Errorf("%s: lease expiry = %v, want now+TTL", l.AttemptID, l.ExpiresAt)
		}
	}
}

func testClaimRespectsLimit(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a", "b", "c", "d", "e"))

	first := mustClaim(t, s, epoch, 2)
	if len(first) != 2 {
		t.Fatalf("claimed %d, want exactly the limit of 2", len(first))
	}
	second := mustClaim(t, s, epoch, 10)
	if len(second) != 3 {
		t.Fatalf("claimed %d of the 3 remaining steps", len(second))
	}
	// Fewer than the limit, including zero, is normal and is not an error.
	if third := mustClaim(t, s, epoch, 10); len(third) != 0 {
		t.Fatalf("claimed %d steps when none were ready", len(third))
	}
}

// testClaimIsExclusive is the in-memory stand-in for SKIP LOCKED: many
// concurrent claimers, and every step handed out exactly once.
func testClaimIsExclusive(t *testing.T, s Store) {
	const steps = 500
	ids := make([]string, steps)
	for i := range ids {
		ids[i] = "s" + itoa(i)
	}
	mustCreate(t, s, job(t, "job_a", epoch, nil, ids...))

	var (
		mu   sync.Mutex
		seen = make(map[string]int, steps)
		wg   sync.WaitGroup
	)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				leases, err := s.Claim(context.Background(), runmesh.ClaimRequest{
					Owner: "test", Limit: 4, LeaseTTL: leaseTTL, Now: epoch,
				})
				if err != nil {
					return
				}
				mu.Lock()
				for _, l := range leases {
					seen[l.StepID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != steps {
		t.Fatalf("claimed %d distinct steps, want %d", len(seen), steps)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("step %s was claimed %d times; claiming must be exclusive", id, n)
		}
	}
}

func testDependencyGating(t *testing.T, s Store) {
	// a -> {b, c} -> d
	deps := map[string][]string{"b": {"a"}, "c": {"a"}, "d": {"b", "c"}}
	mustCreate(t, s, job(t, "job_a", epoch, deps, "a", "b", "c", "d"))

	first := mustClaim(t, s, epoch, 10)
	if len(first) != 1 || first[0].StepID != "a" {
		t.Fatalf("initially claimable = %v, want only [a]", stepIDs(first))
	}
	succeed(t, s, first[0], epoch)

	second := mustClaim(t, s, epoch, 10)
	if got := stepIDs(second); len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("after a succeeded, claimable = %v, want [b c]", got)
	}

	succeed(t, s, second[0], epoch)
	if got := stepIDs(mustClaim(t, s, epoch, 10)); len(got) != 0 {
		t.Fatalf("d became claimable with only one of its two dependencies done: %v", got)
	}
	succeed(t, s, second[1], epoch)

	third := mustClaim(t, s, epoch, 10)
	if got := stepIDs(third); len(got) != 1 || got[0] != "d" {
		t.Fatalf("after b and c succeeded, claimable = %v, want [d]", got)
	}
	succeed(t, s, third[0], epoch)

	if st := jobState(t, s, "job_a"); st != runmesh.Succeeded {
		t.Fatalf("job state = %s, want SUCCEEDED", st)
	}
}

func testFailedDependencyNeverUnblocks(t *testing.T, s Store) {
	j := job(t, "job_a", epoch, map[string][]string{"b": {"a"}}, "a", "b")
	j.OnStepFailure = runmesh.ContinueOnFailure
	mustCreate(t, s, j)

	l := mustClaim(t, s, epoch, 1)[0]
	failStep(t, s, l, epoch, runmesh.Failed)

	if got := stepIDs(mustClaim(t, s, epoch, 10)); len(got) != 0 {
		t.Fatalf("a step whose dependency failed became claimable: %v", got)
	}
	if st := stepState(t, s, "job_a", "b"); st != runmesh.Cancelled {
		t.Fatalf("doomed dependent state = %s, want CANCELLED", st)
	}
}

func testBackoffGate(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	l := mustClaim(t, s, epoch, 1)[0]
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Retrying, CountFail: true,
		NextAttemptAt: epoch.Add(2 * time.Second),
		Error:         &runmesh.ErrorInfo{Code: "boom", Retryable: true, Attempt: 1},
		StartedAt:     epoch, EndedAt: epoch,
	})
	if err != nil {
		t.Fatalf("Finish(RETRYING): %v", err)
	}

	if got := mustClaim(t, s, epoch.Add(time.Second), 1); len(got) != 0 {
		t.Fatal("a retrying step was claimable before its backoff elapsed")
	}
	again := mustClaim(t, s, epoch.Add(2*time.Second), 1)
	if len(again) != 1 {
		t.Fatal("a retrying step was not claimable once its backoff elapsed")
	}
	if again[0].Attempt != 2 || again[0].Failures != 1 {
		t.Fatalf("retry lease has attempt=%d failures=%d, want 2 and 1 "+
			"(attempt names the execution, failures is the budget)",
			again[0].Attempt, again[0].Failures)
	}
}

// testFencingRejectsStaleLease is the zombie-writer regression: a worker whose
// lease was reclaimed must not be able to overwrite the new holder's work.
func testFencingRejectsStaleLease(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	stale := mustClaim(t, s, epoch, 1)[0]
	if _, err := s.ExpireLeases(t.Context(), epoch.Add(leaseTTL+time.Second), 10); err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	fresh := mustClaim(t, s, epoch.Add(leaseTTL+2*time.Second), 1)
	if len(fresh) != 1 {
		t.Fatal("the expired step was not re-claimable")
	}
	if fresh[0].ID == stale.ID {
		t.Fatal("re-claiming reused the fencing token; it must be regenerated")
	}

	before := len(eventTypes(t, s, "job_a"))
	now := epoch.Add(leaseTTL + 3*time.Second)
	for name, err := range map[string]error{
		"Start":   s.Start(t.Context(), stale, now),
		"Finish":  s.Finish(t.Context(), runmesh.Outcome{Lease: stale, State: runmesh.Succeeded, EndedAt: now}),
		"Release": s.Release(t.Context(), stale, "zombie", now),
	} {
		if !errors.Is(err, runmesh.ErrLeaseLost) {
			t.Errorf("%s with a stale lease = %v, want ErrLeaseLost", name, err)
		}
	}
	if _, err := s.Heartbeat(t.Context(), stale, now, now.Add(leaseTTL)); !errors.Is(err, runmesh.ErrLeaseLost) {
		t.Errorf("Heartbeat with a stale lease = %v, want ErrLeaseLost", err)
	}
	if after := len(eventTypes(t, s, "job_a")); after != before {
		t.Errorf("a stale lease holder wrote %d events; it must write nothing", after-before)
	}

	succeed(t, s, fresh[0], now)
	if st := stepState(t, s, "job_a", "a"); st != runmesh.Succeeded {
		t.Fatalf("the valid lease holder could not finish: state = %s", st)
	}
}

// testReleaseRefundsBudgetExpireDoesNot documents the subtlest rule in the
// design by asserting both halves side by side.
//
// A graceful release is OUR failure — a deploy, a drain — so it must cost zero
// retries. A lease that simply expired is different: a worker that reliably
// dies on one step must eventually exhaust MaxAttempts rather than crash-loop
// the fleet on the same poisoned input.
func testReleaseRefundsBudgetExpireDoesNot(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	first := mustClaim(t, s, epoch, 1)[0]
	if err := s.Release(t.Context(), first, "shutdown_drain", epoch); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second := mustClaim(t, s, epoch, 1)[0]
	if second.Failures != 0 {
		t.Errorf("Release spent %d units of retry budget; it must spend none", second.Failures)
	}
	if second.Attempt != 2 {
		t.Errorf("attempt = %d after a release and re-claim, want 2: attempt is monotonic", second.Attempt)
	}

	expireAt := epoch.Add(leaseTTL + time.Second)
	expired, err := s.ExpireLeases(t.Context(), expireAt, 10)
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	if len(expired) != 1 || expired[0].NewState != runmesh.Queued {
		t.Fatalf("expired = %+v, want one step requeued", expired)
	}
	third := mustClaim(t, s, expireAt, 1)[0]
	if third.Failures != 1 {
		t.Errorf("lease expiry spent %d units of budget, want exactly 1", third.Failures)
	}
	if third.Attempt != 3 {
		t.Errorf("attempt = %d, want 3: attempt is never decremented", third.Attempt)
	}
}

func testExpireFailsWhenBudgetExhausted(t *testing.T, s Store) {
	j := job(t, "job_a", epoch, nil, "a")
	j.Steps[0].MaxAttempts = 1
	mustCreate(t, s, j)

	mustClaim(t, s, epoch, 1)
	expired, err := s.ExpireLeases(t.Context(), epoch.Add(leaseTTL+time.Second), 10)
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	if len(expired) != 1 || expired[0].NewState != runmesh.Failed {
		t.Fatalf("expired = %+v, want the step FAILED with its budget exhausted", expired)
	}
	if st := stepState(t, s, "job_a", "a"); st != runmesh.Failed {
		t.Fatalf("step state = %s, want FAILED", st)
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED", st)
	}
}

func testIllegalTransitions(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	l := mustClaim(t, s, epoch, 1)[0]

	// Finish first, then try to Start and Finish again.
	succeed(t, s, l, epoch)
	if err := s.Start(t.Context(), l, epoch); !errors.Is(err, runmesh.ErrLeaseLost) {
		t.Errorf("Start on a finished step = %v, want ErrLeaseLost (the lease was cleared)", err)
	}
	err := s.Finish(t.Context(), runmesh.Outcome{Lease: l, State: runmesh.Failed, EndedAt: epoch})
	if !errors.Is(err, runmesh.ErrLeaseLost) {
		t.Errorf("Finish on a finished step = %v, want ErrLeaseLost", err)
	}

	// A QUEUED step has no lease at all, so Start cannot even be attempted
	// with a valid token: that is the point of the fencing design.
	mustCreate(t, s, job(t, "job_b", epoch, nil, "a"))
	bogus := runmesh.Lease{ID: "lse_bogus", JobID: "job_b", StepID: "a"}
	if err := s.Start(t.Context(), bogus, epoch); !errors.Is(err, runmesh.ErrLeaseLost) {
		t.Errorf("Start on a QUEUED step = %v, want ErrLeaseLost", err)
	}
}

func testHeartbeatDeliversCancel(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	l := mustClaim(t, s, epoch, 1)[0]
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dir, err := s.Heartbeat(t.Context(), l, epoch, epoch.Add(leaseTTL))
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if dir.Cancel {
		t.Fatal("heartbeat reported a cancellation nobody requested")
	}

	if _, err := s.RequestCancel(t.Context(), "job_a", runmesh.CancelUser, epoch); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	dir, err = s.Heartbeat(t.Context(), l, epoch, epoch.Add(leaseTTL))
	if err != nil {
		t.Fatalf("Heartbeat after cancel: %v", err)
	}
	if !dir.Cancel || dir.Reason != runmesh.CancelUser {
		t.Fatalf("directive = %+v, want Cancel with reason %q", dir, runmesh.CancelUser)
	}
}

func testCancelLeavesLeasedStepsAlone(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a", "b"))
	l := mustClaim(t, s, epoch, 1)[0] // claims "a"
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}

	j, err := s.RequestCancel(t.Context(), "job_a", runmesh.CancelUser, epoch)
	if err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if j.State != runmesh.Running {
		t.Fatalf("job state = %s immediately after cancel, want RUNNING until in-flight steps drain", j.State)
	}
	if j.CancelRequestedAt == nil {
		t.Fatal("cancel_requested_at was not set")
	}
	if st := stepState(t, s, "job_a", "a"); st != runmesh.Running {
		t.Fatalf("leased step state = %s, want RUNNING: only a heartbeat may stop it", st)
	}
	if st := stepState(t, s, "job_a", "b"); st != runmesh.Cancelled {
		t.Fatalf("unstarted step state = %s, want CANCELLED", st)
	}

	// The in-flight step settles; only then does the job finalise.
	err = s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Cancelled,
		Error:     &runmesh.ErrorInfo{Code: runmesh.CodeCancelled, Attempt: 1},
		StartedAt: epoch, EndedAt: epoch.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("Finish(CANCELLED): %v", err)
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Cancelled {
		t.Fatalf("job state = %s once drained, want CANCELLED", st)
	}
}

func testCancelBlocksClaiming(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a", "b"))
	if _, err := s.RequestCancel(t.Context(), "job_a", runmesh.CancelUser, epoch); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if got := mustClaim(t, s, epoch, 10); len(got) != 0 {
		t.Fatalf("claimed %d steps from a cancelled job", len(got))
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Cancelled {
		t.Fatalf("job with nothing in flight = %s, want CANCELLED immediately", st)
	}
	// Cancelling twice is idempotent, not an error.
	if _, err := s.RequestCancel(t.Context(), "job_a", runmesh.CancelUser, epoch); !errors.Is(err, runmesh.ErrConflict) {
		t.Fatalf("cancelling a terminal job = %v, want ErrConflict", err)
	}
}

func testFailFastCancelsSiblings(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a", "b", "c"))

	l := mustClaim(t, s, epoch, 1)[0] // "a"
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}
	err := s.Finish(t.Context(), runmesh.Outcome{
		Lease: l, State: runmesh.Failed, CountFail: true,
		Error:     &runmesh.ErrorInfo{Code: "boom", Message: "injected", Attempt: 1},
		StartedAt: epoch, EndedAt: epoch,
	})
	if err != nil {
		t.Fatalf("Finish(FAILED): %v", err)
	}

	for _, id := range []string{"b", "c"} {
		if st := stepState(t, s, "job_a", id); st != runmesh.Cancelled {
			t.Errorf("sibling %s = %s under fail_fast, want CANCELLED", id, st)
		}
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED (the failing step's state wins, not the cancelled siblings')", st)
	}
}

func testContinueOnFailureDoomsDependents(t *testing.T, s Store) {
	// a -> b -> c, plus an independent d that must still run.
	deps := map[string][]string{"b": {"a"}, "c": {"b"}}
	j := job(t, "job_a", epoch, deps, "a", "b", "c", "d")
	j.OnStepFailure = runmesh.ContinueOnFailure
	mustCreate(t, s, j)

	leases := mustClaim(t, s, epoch, 10)
	if got := stepIDs(leases); len(got) != 2 {
		t.Fatalf("claimable = %v, want [a d]", got)
	}
	failStep(t, s, leases[0], epoch, runmesh.Failed) // a fails

	for _, id := range []string{"b", "c"} {
		if st := stepState(t, s, "job_a", id); st != runmesh.Cancelled {
			t.Errorf("transitive dependent %s = %s, want CANCELLED", id, st)
		}
	}
	if st := stepState(t, s, "job_a", "d"); st != runmesh.Scheduled {
		t.Errorf("independent step d = %s; continue_on_failure must not touch it", st)
	}

	succeed(t, s, leases[1], epoch) // d succeeds
	if st := jobState(t, s, "job_a"); st != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED once everything settled", st)
	}
}

func testRollupPrecedence(t *testing.T, s Store) {
	// TIMED_OUT outranks FAILED, which outranks CANCELLED.
	j := job(t, "job_a", epoch, nil, "a", "b")
	j.OnStepFailure = runmesh.ContinueOnFailure
	mustCreate(t, s, j)

	leases := mustClaim(t, s, epoch, 2)
	failStep(t, s, leases[0], epoch, runmesh.Failed)
	failStep(t, s, leases[1], epoch, runmesh.TimedOut)

	if st := jobState(t, s, "job_a"); st != runmesh.TimedOut {
		t.Fatalf("job state = %s, want TIMED_OUT (it outranks FAILED)", st)
	}
}

func testEventSequences(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	l := mustClaim(t, s, epoch, 1)[0]
	succeed(t, s, l, epoch)

	page, err := s.JobEvents(t.Context(), "job_a", 0, 100)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	want := []runmesh.EventType{
		runmesh.JobCreated,
		runmesh.StepScheduled,
		runmesh.JobStarted,
		runmesh.StepStarted,
		runmesh.StepFinished,
		runmesh.JobFinished,
	}
	if got := eventTypes(t, s, "job_a"); !sameTypes(got, want) {
		t.Fatalf("timeline = %v,\n           want %v", got, want)
	}
	for i, e := range page.Events {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d; the per-job sequence must be 1-based and gap-free", i, e.Seq)
		}
		if e.JobID != "job_a" {
			t.Fatalf("event %d has job_id %q", i, e.JobID)
		}
		if i > 0 && e.GlobalSeq <= page.Events[i-1].GlobalSeq {
			t.Fatalf("global_seq is not strictly increasing at event %d", i)
		}
	}
}

func testEventPaging(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	succeed(t, s, mustClaim(t, s, epoch, 1)[0], epoch)

	var seen []runmesh.EventType
	var after uint64
	for range 10 {
		page, err := s.JobEvents(t.Context(), "job_a", after, 2)
		if err != nil {
			t.Fatalf("JobEvents(after=%d): %v", after, err)
		}
		if len(page.Events) == 0 {
			break
		}
		for _, e := range page.Events {
			seen = append(seen, e.Type)
		}
		if page.NextAfter <= after {
			t.Fatalf("cursor did not advance: after=%d next_after=%d", after, page.NextAfter)
		}
		after = page.NextAfter
	}
	if len(seen) != 6 {
		t.Fatalf("paging yielded %d events, want the full timeline of 6", len(seen))
	}
}

func testTailEvents(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	mustCreate(t, s, job(t, "job_b", epoch, nil, "a"))

	all, err := s.TailEvents(t.Context(), 0, 100)
	if err != nil {
		t.Fatalf("TailEvents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("TailEvents returned %d events across two jobs, want 2", len(all))
	}
	rest, err := s.TailEvents(t.Context(), all[0].GlobalSeq, 100)
	if err != nil {
		t.Fatalf("TailEvents(after): %v", err)
	}
	if len(rest) != 1 || rest[0].GlobalSeq != all[1].GlobalSeq {
		t.Fatalf("resuming from a global cursor returned %d events, want exactly the 1 that followed", len(rest))
	}
}

// testSubscribeDropsRatherThanBlocks pins the rule that observability must
// never apply backpressure to execution.
func testSubscribeDropsRatherThanBlocks(t *testing.T, s Store) {
	ch, unsubscribe := s.Subscribe(1)
	defer unsubscribe()

	for i := range 200 {
		mustCreate(t, s, job(t, "job_"+itoa(i), epoch, nil, "a"))
	}
	// The subscriber never read, and the store did not stall: reaching here at
	// all is the assertion.
	select {
	case <-ch:
	default:
		t.Fatal("subscriber received nothing at all")
	}
}

func testSubscribeUnsubscribeIsRaceFree(t *testing.T, s Store) {
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, unsubscribe := s.Subscribe(4)
			unsubscribe()
			unsubscribe() // must be idempotent, never a double close
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.CreateJob(context.Background(), job(t, "job_"+itoa(i), epoch, nil, "a"), "")
		}()
	}
	wg.Wait()
}

// testReadyHintIsLossy asserts that correctness never depends on the wake-up
// hint — which is what lets a PostgreSQL store return a nil channel.
func testReadyHintIsLossy(t *testing.T, s Store) {
	for i := range 5 {
		mustCreate(t, s, job(t, "job_"+itoa(i), epoch, nil, "a"))
	}
	// Drain the hint channel entirely; work must still be claimable.
	for {
		select {
		case <-s.Ready():
			continue
		default:
		}
		break
	}
	if got := mustClaim(t, s, epoch, 10); len(got) != 5 {
		t.Fatalf("claimed %d steps after discarding every hint, want 5", len(got))
	}
}

func testQueueDepthMatchesClaimPredicate(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, map[string][]string{"b": {"a"}}, "a", "b"))

	depth, err := s.QueueDepth(t.Context(), epoch)
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depth != 1 {
		t.Fatalf("QueueDepth = %d, want 1: only the unblocked step counts", depth)
	}

	succeed(t, s, mustClaim(t, s, epoch, 1)[0], epoch)
	if depth, _ = s.QueueDepth(t.Context(), epoch); depth != 1 {
		t.Fatalf("QueueDepth = %d after the dependency succeeded, want 1", depth)
	}
	succeed(t, s, mustClaim(t, s, epoch, 1)[0], epoch)
	if depth, _ = s.QueueDepth(t.Context(), epoch); depth != 0 {
		t.Fatalf("QueueDepth = %d with the job finished, want 0", depth)
	}
}

func testListJobsPaging(t *testing.T, s Store) {
	for i := range 5 {
		mustCreate(t, s, job(t, "job_"+itoa(i), epoch.Add(time.Duration(i)*time.Second), nil, "a"))
	}

	first, err := s.ListJobs(t.Context(), runmesh.JobFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(first.Jobs) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %d jobs, cursor %q", len(first.Jobs), first.NextCursor)
	}
	second, err := s.ListJobs(t.Context(), runmesh.JobFilter{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("ListJobs(cursor): %v", err)
	}
	if len(second.Jobs) != 2 {
		t.Fatalf("second page = %d jobs, want 2", len(second.Jobs))
	}
	for _, a := range first.Jobs {
		for _, b := range second.Jobs {
			if a.ID == b.ID {
				t.Fatalf("job %s appeared on both pages; keyset pagination must not overlap", a.ID)
			}
		}
	}

	filtered, err := s.ListJobs(t.Context(), runmesh.JobFilter{States: []runmesh.State{runmesh.Succeeded}})
	if err != nil {
		t.Fatalf("ListJobs(state): %v", err)
	}
	if len(filtered.Jobs) != 0 {
		t.Fatalf("state filter returned %d jobs, want 0", len(filtered.Jobs))
	}
}

func testClosedStoreRejectsEverything(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close twice: %v", err)
	}

	l := runmesh.Lease{ID: "lse_x", JobID: "job_a", StepID: "a"}
	checks := map[string]error{
		"Ping":    s.Ping(t.Context()),
		"Start":   s.Start(t.Context(), l, epoch),
		"Finish":  s.Finish(t.Context(), runmesh.Outcome{Lease: l, State: runmesh.Succeeded, EndedAt: epoch}),
		"Release": s.Release(t.Context(), l, "x", epoch),
	}
	if _, err := s.Job(t.Context(), "job_a"); err != nil {
		checks["Job"] = err
	}
	if _, err := s.Claim(t.Context(), runmesh.ClaimRequest{Owner: "t", Limit: 1, Now: epoch}); err != nil {
		checks["Claim"] = err
	}
	if _, err := s.ExpireLeases(t.Context(), epoch, 1); err != nil {
		checks["ExpireLeases"] = err
	}
	if _, err := s.Heartbeat(t.Context(), l, epoch, epoch); err != nil {
		checks["Heartbeat"] = err
	}
	if _, err := s.QueueDepth(t.Context(), epoch); err != nil {
		checks["QueueDepth"] = err
	}
	for name, err := range checks {
		if !errors.Is(err, runmesh.ErrClosed) {
			t.Errorf("%s on a closed store = %v, want ErrClosed", name, err)
		}
	}
}

// ------------------------------------------------------------------ helpers

func stepIDs(leases []runmesh.Lease) []string {
	out := make([]string, len(leases))
	for i, l := range leases {
		out[i] = l.StepID
	}
	return out
}

func sameTypes(got, want []runmesh.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// itoa keeps generated step ids inside the domain's id pattern without
// dragging strconv into every call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ------------------------------------------------- review regressions

// testFailFastAppliesToLeaseExpiryToo is the regression for a HIGH-severity
// bug an adversarial review found: the fail-fast trigger lived in Finish, so a
// step driven to FAILED by the lease sweep instead — a dead worker exhausting
// its retry budget — raised no cancel flag and doomed nothing. Its dependents
// sat QUEUED for ever and the job never reached a terminal state.
//
// The fix moved the trigger into the one function every writer ends in, which
// is exactly the kind of thing this suite exists to hold: a pgstore that
// re-implements ExpireLeases without it fails here.
func testFailFastAppliesToLeaseExpiryToo(t *testing.T, s Store) {
	j := job(t, "job_a", epoch, map[string][]string{"b": {"a"}}, "a", "b")
	j.Steps[0].MaxAttempts = 1 // one expiry is enough to exhaust it
	mustCreate(t, s, j)

	// Claim without ever settling: what a killed worker leaves behind.
	if got := mustClaim(t, s, epoch, 1); len(got) != 1 {
		t.Fatalf("claimed %d steps, want 1", len(got))
	}

	expireAt := epoch.Add(leaseTTL + time.Second)
	expired, err := s.ExpireLeases(t.Context(), expireAt, 10)
	if err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	if len(expired) != 1 || expired[0].NewState != runmesh.Failed {
		t.Fatalf("expired = %+v, want the step FAILED with its budget exhausted", expired)
	}

	if st := stepState(t, s, "job_a", "a"); st != runmesh.Failed {
		t.Fatalf("step a = %s, want FAILED", st)
	}
	if st := stepState(t, s, "job_a", "b"); st != runmesh.Cancelled {
		t.Errorf("dependent step b = %s, want CANCELLED: a fail-fast job must not "+
			"leave dependents claimable-never", st)
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED. A job whose step failed via lease "+
			"expiry must still reach a terminal state", st)
	}
	if got := mustClaim(t, s, expireAt, 10); len(got) != 0 {
		t.Errorf("a terminal job still offered %d claimable steps", len(got))
	}
}

// testReleaseOnACancelledJobDoesNotStrand: releasing a step during a drain
// puts it back in QUEUED, but the claim predicate excludes cancel-flagged
// jobs — so without the reconcile that follows, the step would sit QUEUED for
// ever and its job would never become quiescent.
func testReleaseOnACancelledJobDoesNotStrand(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	l := mustClaim(t, s, epoch, 1)[0]
	if err := s.Start(t.Context(), l, epoch); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := s.RequestCancel(t.Context(), "job_a", runmesh.CancelUser, epoch); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// The drain deadline expires and the worker hands the step back.
	if err := s.Release(t.Context(), l, "shutdown_drain", epoch.Add(time.Second)); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if st := stepState(t, s, "job_a", "a"); st != runmesh.Cancelled {
		t.Errorf("released step = %s, want CANCELLED: it can never be claimed again", st)
	}
	if st := jobState(t, s, "job_a"); st != runmesh.Cancelled {
		t.Fatalf("job state = %s, want CANCELLED. A step released onto a cancelled "+
			"job must not leave it stuck non-terminal", st)
	}
}

// testSubscribersAreIsolated: a delivered event must be the subscriber's own
// copy. An Event is a value, but its Error pointer and Attrs map are not, so a
// shared one lets a subscriber corrupt the store's own timeline — something a
// SQL store, which decodes rows per caller, could never reproduce.
func testSubscribersAreIsolated(t *testing.T, s Store) {
	first, unsub1 := s.Subscribe(16)
	defer unsub1()
	second, unsub2 := s.Subscribe(16)
	defer unsub2()

	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))
	l := mustClaim(t, s, epoch, 1)[0]
	failStep(t, s, l, epoch, runmesh.Failed)

	// Find a delivered event carrying an error and mutate everything on it.
	var mutated bool
	for range 16 {
		select {
		case e := <-first:
			if e.Error == nil {
				continue
			}
			e.Error.Code = "corrupted"
			e.Error.Message = "corrupted"
			if e.Attrs != nil {
				e.Attrs["corrupted"] = true
			}
			mutated = true
		default:
		}
		if mutated {
			break
		}
	}
	if !mutated {
		t.Skip("no error-carrying event was delivered; nothing to isolate")
	}

	// The second subscriber's copy must be untouched.
	for range 16 {
		select {
		case e := <-second:
			if e.Error != nil && e.Error.Code == "corrupted" {
				t.Fatal("subscribers share one *ErrorInfo; mutating a delivered event " +
					"reached another subscriber")
			}
		default:
		}
	}
	// And so must the store's own timeline.
	page, err := s.JobEvents(t.Context(), "job_a", 0, 100)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	for _, e := range page.Events {
		if e.Error != nil && e.Error.Code == "corrupted" {
			t.Fatal("a subscriber mutating a delivered event corrupted the stored timeline")
		}
		if _, bad := e.Attrs["corrupted"]; bad {
			t.Fatal("a subscriber mutating a delivered event's Attrs corrupted the stored timeline")
		}
	}
}

// testAbsurdEventCursorDoesNotReportAFalseGap: Seq is a uint64, so a cursor of
// MaxUint64 used to wrap to zero in the truncation check and report a gap on a
// timeline that has none.
func testAbsurdEventCursorDoesNotReportAFalseGap(t *testing.T, s Store) {
	mustCreate(t, s, job(t, "job_a", epoch, nil, "a"))

	page, err := s.JobEvents(t.Context(), "job_a", math.MaxUint64, 10)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if page.Truncated {
		t.Error("an out-of-range cursor reported truncated on a timeline with no gap")
	}
	if len(page.Events) != 0 {
		t.Errorf("a cursor past the end returned %d events", len(page.Events))
	}

	// A cursor of zero on a complete timeline is likewise not truncated.
	page, err = s.JobEvents(t.Context(), "job_a", 0, 10)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	if page.Truncated {
		t.Error("a fresh timeline reported truncated")
	}
}
