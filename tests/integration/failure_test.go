//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// orphan puts one step into the state a dead worker leaves behind: SCHEDULED
// then RUNNING, under a lease belonging to an owner that will never heartbeat
// again.
//
// It goes through the store rather than through the engine, and that is the
// honest way to do it rather than a shortcut. A lease IS the liveness claim —
// the whole design says a worker is alive if and only if it is renewing — so a
// lease nobody renews is precisely and completely what a dead worker leaves.
// Killing a goroutine would produce something different and less useful: the
// engine's drain RELEASES its leases on the way out, refunding the retry budget,
// which is the opposite of the situation these tests are about.
func orphan(t *testing.T, h *harness, jobID, owner string) runmesh.Lease {
	t.Helper()

	now := clock.System().Now()
	leases, err := h.store.Claim(t.Context(), runmesh.ClaimRequest{
		Owner:    owner,
		Limit:    1,
		LeaseTTL: runtimeConfig.LeaseTTL,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("claiming a step as %q: %v", owner, err)
	}
	if len(leases) != 1 {
		t.Fatalf("claimed %d steps as %q, want exactly 1: nothing was claimable, so the "+
			"rest of this test would be asserting against a queue that never moved", len(leases), owner)
	}
	if leases[0].JobID != jobID {
		t.Fatalf("claimed a step of job %s, want %s", leases[0].JobID, jobID)
	}
	if err := h.store.Start(t.Context(), leases[0], now); err != nil {
		t.Fatalf("starting the orphaned step: %v", err)
	}
	return leases[0]
}

// TestReconcilerRecoversAStepItsWorkerNeverFinished is the recovery claim, end
// to end and through the public API.
//
// A step is running, under a lease held by a worker that is gone. Nothing in the
// runtime knows that; there is no health check on a worker and there is
// deliberately no heartbeat FROM the reconciler TO anything. The only mechanism
// is time: the lease stops being renewed, it expires, the sweep reclaims it, and
// the step goes back into the queue for somebody else. That is the entire
// crash-recovery story, and this is it happening.
//
// The assertion is on the TIMELINE as well as on the final state, because the
// final state alone cannot tell a recovery from a coincidence. A job that ends
// SUCCEEDED after a reclaimed lease and a job that ends SUCCEEDED because
// nothing ever went wrong are the same job by every other measure;
// STEP_LEASE_EXPIRED is the difference.
func TestReconcilerRecoversAStepItsWorkerNeverFinished(t *testing.T) {
	t.Parallel()
	for _, be := range backends(t) {
		t.Run("store="+be.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, be, options{})

			// A chain, so the test can also show that the DAG resumes rather
			// than merely that one step came back: `after` cannot run until
			// `work` succeeds, and `work` is the step whose worker dies.
			job := h.submit(`{
			  "name": "recovery",
			  "steps": [
			    {"id": "work",  "tool": "echo", "params": {"msg": "hi"}, "max_attempts": 3},
			    {"id": "after", "tool": "echo", "params": {"msg": "bye"}, "depends_on": ["work"]}
			  ]
			}`)

			// Break it BEFORE the runtime is allowed to touch it. Starting the
			// engine first would be a race with the dispatcher for the same
			// step, and a flaky failure test is worse than none.
			lease := orphan(t, h, job.ID, "worker-that-died")
			h.start()

			final := h.waitForJobState(job.ID, "SUCCEEDED", 20*time.Second)

			work := final.Steps[final.step(t, "work")]
			if work.Attempt < 2 {
				t.Errorf("work.attempt = %d, want at least 2: the abandoned attempt must not be "+
					"reused, because an attempt names one execution", work.Attempt)
			}
			if work.Failures != 1 {
				t.Errorf("work.failures = %d, want exactly 1. A lease that simply expired spends "+
					"one unit of budget: a worker that reliably dies on one step has to exhaust "+
					"max_attempts rather than crash-loop the fleet.", work.Failures)
			}
			if got := final.Steps[final.step(t, "after")].State; got != "SUCCEEDED" {
				t.Errorf("after.state = %s, want SUCCEEDED: recovering the step is only half of "+
					"recovery — the DAG downstream of it has to resume too", got)
			}

			events := h.timeline(job.ID)
			if !hasEvent(events, runmesh.StepLeaseExpired, "work") {
				t.Errorf("timeline = %v, want a STEP_LEASE_EXPIRED for work. Without it this job "+
					"succeeded for some reason other than the one under test.", eventTypes(events))
			}
			// The reclaim has to name the owner that vanished. It is what an
			// operator greps for after an incident, and an event that recorded
			// only that "a lease expired" would leave them unable to tell one
			// dead replica from a fleet-wide problem.
			if !reclaimedFrom(events, lease.Owner) {
				t.Errorf("no STEP_LEASE_EXPIRED names the previous owner %q; the timeline cannot "+
					"say which replica died", lease.Owner)
			}
		})
	}
}

// TestLeaseExpirySpendsBudgetAndRequeues isolates the arithmetic the test above
// can only observe after the fact.
//
// It never starts the worker pool. The engine's reconciler is driven by ONE
// explicit call to Reconcile, so what is asserted is the state of the step the
// instant the sweep finished — QUEUED, one unit of budget gone, the attempt
// counter untouched — rather than whatever state a racing dispatcher left it in
// a moment later.
//
// The second half is the asymmetry that matters more than the first. Budget is
// finite, so a lease expiry is not always a retry: when the step has no budget
// left the sweep drives it to FAILED instead, and — because fail-fast is a
// property of the transition rather than of Finish's call site — the dependents
// of that step are cancelled by the same sweep. That second half is a regression
// test in disguise; the Week-1 review found fail-fast living in Finish, where a
// step driven to FAILED by the RECONCILER raised no cancel flag and its
// dependents sat QUEUED for ever.
func TestLeaseExpirySpendsBudgetAndRequeues(t *testing.T) {
	t.Parallel()
	for _, be := range backends(t) {
		t.Run("store="+be.name, func(t *testing.T) {
			t.Parallel()

			t.Run("budget=remaining", func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, be, options{})
				job := h.submit(`{
				  "name": "expiry-requeues",
				  "steps": [{"id": "work", "tool": "echo", "max_attempts": 3}]
				}`)
				orphan(t, h, job.ID, "worker-that-died")

				sweepAfterExpiry(t, h, 1)

				got := h.job(job.ID)
				work := got.Steps[got.step(t, "work")]
				if work.State != "QUEUED" {
					t.Errorf("work.state = %s, want QUEUED: a lease with budget left goes back "+
						"into the queue for another worker", work.State)
				}
				if work.Failures != 1 {
					t.Errorf("work.failures = %d, want 1: an expiry SPENDS budget, unlike a "+
						"Release, which refunds it", work.Failures)
				}
				if work.Attempt != 1 {
					t.Errorf("work.attempt = %d, want 1: the expiry returns the step to the "+
						"queue, and the next CLAIM is what names the next attempt", work.Attempt)
				}
				events := h.timeline(job.ID)
				if !hasEvent(events, runmesh.StepLeaseExpired, "work") {
					t.Errorf("timeline = %v, want STEP_LEASE_EXPIRED", eventTypes(events))
				}
			})

			t.Run("budget=exhausted", func(t *testing.T) {
				t.Parallel()
				h := newHarness(t, be, options{})
				// One attempt, so the claim the orphan takes is the only one
				// this step will ever get and the expiry has nowhere to go but
				// FAILED.
				job := h.submit(`{
				  "name": "expiry-exhausts",
				  "steps": [
				    {"id": "work",  "tool": "echo", "max_attempts": 1},
				    {"id": "after", "tool": "echo", "depends_on": ["work"]}
				  ]
				}`)
				orphan(t, h, job.ID, "worker-that-died")

				sweepAfterExpiry(t, h, 1)

				got := h.job(job.ID)
				work := got.Steps[got.step(t, "work")]
				if work.State != "FAILED" {
					t.Errorf("work.state = %s, want FAILED: with no budget left an expiry is "+
						"terminal, or a worker that reliably dies on one step crash-loops the "+
						"fleet for ever", work.State)
				}
				after := got.Steps[got.step(t, "after")]
				if after.State == "QUEUED" {
					t.Errorf("after.state = QUEUED after its dependency failed by lease expiry. " +
						"This is the Week-1 regression: fail-fast lived in Finish, so a step " +
						"driven to FAILED by the RECONCILER raised no cancel flag and its " +
						"dependents waited for ever.")
				}
				if got.State != "FAILED" {
					t.Errorf("job.state = %s, want FAILED: a job whose only path is dead has "+
						"terminalised, and a job that never terminalises is a job nobody can "+
						"garbage-collect", got.State)
				}
			})
		})
	}
}

// sweepAfterExpiry waits out the lease and runs exactly one reconciler pass,
// asserting it found what the caller expected.
//
// The wait is elapsed real time and cannot be anything else: the lease deadline
// lives in the store as a timestamp, the engine compares it against its own
// clock, and there is no fake clock in an integration harness to advance. The
// margin is deliberately a whole extra lease TTL rather than a few
// milliseconds — a sweep that ran one millisecond early would find nothing and
// fail this test for a reason that has nothing to do with the behaviour.
func sweepAfterExpiry(t *testing.T, h *harness, want int) {
	t.Helper()

	clk := clock.System()
	deadline := clk.Now().Add(10 * time.Second)
	time.Sleep(2 * runtimeConfig.LeaseTTL)

	for {
		if n := h.eng.Reconcile(t.Context()); n >= want {
			return
		}
		if clk.Now().After(deadline) {
			t.Fatalf("the reconciler reclaimed nothing within 10s, though the lease TTL is %s "+
				"and it has long since expired", runtimeConfig.LeaseTTL)
		}
		poll()
	}
}

// TestCancellingAJobInFlightFailsFastToDependents cancels work that is actually
// running, and follows the consequence downstream.
//
// The mechanism under test is the one the design chose over every alternative: a
// cancellation is not a signal sent to a worker, because the API call that
// requests it routinely lands on a DIFFERENT PROCESS from the one executing the
// step. It is a flag in the store, and it travels back on the next heartbeat —
// the same round trip that renews the lease. That is why the heartbeat carries a
// Directive at all, and it is why this test compresses the heartbeat interval
// rather than the lease.
//
// Two things are asserted, and the second is the one worth having. First, the
// running step stops. Second, the steps that were merely WAITING on it do not
// sit QUEUED for ever: cancellation propagates to unstarted steps in the same
// transition, so the whole job terminalises rather than half of it.
func TestCancellingAJobInFlightFailsFastToDependents(t *testing.T) {
	t.Parallel()
	for _, be := range backends(t) {
		t.Run("store="+be.name, func(t *testing.T) {
			t.Parallel()
			// One worker, so `slow` is definitely the step in flight and the
			// two dependents are definitely still queued when the cancellation
			// lands. With four workers the dependents would be blocked by the
			// DAG rather than by capacity, which happens to be the same outcome
			// here and would make the test pass for a weaker reason.
			h := newHarness(t, be, options{workers: 1})

			job := h.submit(`{
			  "name": "cancel-in-flight",
			  "steps": [
			    {"id": "slow",   "tool": "sleep", "params": {"duration": "10s"}},
			    {"id": "left",   "tool": "echo",  "depends_on": ["slow"]},
			    {"id": "right",  "tool": "echo",  "depends_on": ["slow"]},
			    {"id": "report", "tool": "echo",  "depends_on": ["left", "right"]}
			  ]
			}`)
			h.start()

			// Cancel only once the step is genuinely executing. Cancelling a
			// QUEUED job is a different and much easier path — nothing has to
			// be told anything — and it is not the one this test is named for.
			h.waitForStepState(job.ID, "slow", "RUNNING", 15*time.Second)

			status, body := h.do(http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel",
				`{"reason": "operator stopped it"}`)
			if status != http.StatusAccepted && status != http.StatusOK {
				t.Fatalf("POST cancel = %d, want 202 or 200. body: %s", status, body)
			}

			// Ten seconds is far longer than the sleep tool's own deadline is
			// short, which is the point: if cancellation did not reach the
			// running step, this would time out rather than pass late.
			final := h.waitForJobState(job.ID, "CANCELLED", 15*time.Second)

			if got := final.Steps[final.step(t, "slow")].State; got != "CANCELLED" {
				t.Errorf("slow.state = %s, want CANCELLED: the directive rides back on the "+
					"heartbeat, and a step that ignored it would have run its full ten seconds", got)
			}
			for _, id := range []string{"left", "right", "report"} {
				got := final.Steps[final.step(t, id)].State
				if got == "QUEUED" || got == "SCHEDULED" || got == "RUNNING" {
					t.Errorf("%s.state = %s after the job was cancelled. Fail-fast has to reach "+
						"the dependents, or a cancelled job leaves steps that will never run and "+
						"never terminalise.", id, got)
				}
				if got != "CANCELLED" {
					t.Errorf("%s.state = %s, want CANCELLED", id, got)
				}
			}

			events := h.timeline(job.ID)
			if !hasEvent(events, runmesh.JobCancelRequested, "") {
				t.Errorf("timeline = %v, want JOB_CANCEL_REQUESTED", eventTypes(events))
			}
			if !hasEvent(events, runmesh.JobFinished, "") {
				t.Errorf("timeline = %v, want JOB_FINISHED: a cancelled job still terminalises",
					eventTypes(events))
			}
		})
	}
}

// TestSubmittingPastTheQueueLimitPushesBack is admission control, which is the
// only backpressure this system has.
//
// A runtime that accepts unbounded work does not stay polite for long: it
// accepts, and accepts, and then falls over holding a queue nobody asked for.
// RUNMESH_MAX_QUEUE_DEPTH is the ceiling, it is checked BEFORE persistence, and
// the refusal is a 429 with a Retry-After — a caller is told to come back rather
// than told it did something wrong.
//
// The interesting assertion is the last one. A ceiling that refuses is easy; a
// ceiling that refuses FOR EVER is a latch, and a latch is an outage. So the
// test drains the queue by starting the runtime and requires the very next
// submission to be accepted.
func TestSubmittingPastTheQueueLimitPushesBack(t *testing.T) {
	t.Parallel()
	for _, be := range backends(t) {
		t.Run("store="+be.name, func(t *testing.T) {
			t.Parallel()
			const limit = 12
			// The engine is deliberately not started, so nothing drains the
			// queue while it is being filled. Racing the dispatcher would make
			// the depth at which the refusal arrives nondeterministic.
			h := newHarness(t, be, options{maxQueueDepth: limit})

			const perJob = 5
			var accepted, refused int
			var refusal []byte

			// Submit past the ceiling. The loop is bounded well above what the
			// limit permits so a server that never refuses fails here with a
			// count rather than by running for ever.
			for i := range 20 {
				status, body := h.do(http.MethodPost, "/api/v1/jobs", flatPlan("admission-"+strconv.Itoa(i), perJob))
				switch status {
				case http.StatusCreated:
					accepted++
				case http.StatusTooManyRequests:
					refused++
					if refusal == nil {
						refusal = body
					}
				default:
					t.Fatalf("POST /api/v1/jobs = %d, want 201 or 429. body: %s", status, body)
				}
			}

			if refused == 0 {
				t.Fatalf("%d jobs of %d steps were all accepted against a ceiling of %d: "+
					"admission control never engaged", accepted, perJob, limit)
			}
			if accepted == 0 {
				t.Fatal("not one job was accepted; the ceiling refused an empty queue")
			}
			// The ceiling is enforced before persistence, so it has to hold as
			// an actual ceiling and not merely as an eventual one.
			if depth := accepted * perJob; depth > limit {
				t.Errorf("%d steps were admitted against a ceiling of %d", depth, limit)
			}

			// A refusal has to say what it is. resource_exhausted is what
			// distinguishes "come back later" from "your request was wrong", and
			// a client that cannot tell them apart will retry the wrong one.
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(refusal, &env); err != nil {
				t.Fatalf("the 429 body is not the error envelope: %v (body %s)", err, refusal)
			}
			if env.Error.Code != "resource_exhausted" {
				t.Errorf("429 code = %q, want resource_exhausted", env.Error.Code)
			}

			// And now the half that says it is a ceiling rather than a latch.
			h.start()
			deadline := clock.System().Now().Add(30 * time.Second)
			for {
				status, body := h.do(http.MethodPost, "/api/v1/jobs", flatPlan("admission-after-drain", perJob))
				if status == http.StatusCreated {
					break
				}
				if status != http.StatusTooManyRequests {
					t.Fatalf("POST after the drain = %d, body %s", status, body)
				}
				if clock.System().Now().After(deadline) {
					t.Fatal("submissions were still refused 30s after the runtime started draining " +
						"the queue: admission control is a latch, not a ceiling")
				}
				poll()
			}
		})
	}
}

// flatPlan is n independent steps, so the whole plan becomes claimable at once
// and the queue depth it contributes is exactly n.
func flatPlan(name string, n int) string {
	plan := map[string]any{"name": name}
	steps := make([]map[string]any, 0, n)
	for i := range n {
		steps = append(steps, map[string]any{
			"id":     "s" + strconv.Itoa(i),
			"tool":   "echo",
			"params": map[string]any{"msg": "admission"},
		})
	}
	plan["steps"] = steps
	out, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// reclaimedFrom reports whether any STEP_LEASE_EXPIRED names the given previous
// owner. The attribute is what an operator reads to tell one dead replica from a
// fleet-wide failure.
func reclaimedFrom(events []runmesh.Event, owner string) bool {
	for _, e := range events {
		if e.Type != runmesh.StepLeaseExpired {
			continue
		}
		for _, v := range e.Attrs {
			if s, ok := v.(string); ok && s == owner {
				return true
			}
		}
	}
	return false
}
