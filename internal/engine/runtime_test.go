package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/engine"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

var epoch = time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

// ------------------------------------------------------------------ harness

type harness struct {
	t     *testing.T
	clk   *clock.Fake
	store *memstore.Store
	reg   tools.Registry
	eng   *engine.Engine

	events <-chan runmesh.Event
	unsub  func()

	// seen buffers every event received so far, and used marks the ones an
	// await has already matched. Buffering rather than consuming matters: one
	// await often has to read past events another await is still looking for,
	// and dropping those would make assertion order significant.
	seenMu sync.Mutex
	seen   []runmesh.Event
	used   []bool
}

// option mutates the engine's configuration OR its dependencies before
// construction. Both, because the execution policy arrived in Week 4 as a
// dependency rather than a config field, and a test that needs to deny a tool
// needs to reach it.
type option func(*engine.Config, *engine.Deps)

func withWorkers(n int) option {
	return func(c *engine.Config, _ *engine.Deps) { c.Workers = n; c.ClaimBatch = n }
}

func withBackoff(b engine.Backoff) option {
	return func(c *engine.Config, _ *engine.Deps) { c.Backoff = b }
}

func withSandbox(s engine.Sandbox) option {
	return func(_ *engine.Config, d *engine.Deps) { d.Sandbox = s }
}

func withExecutor(e tools.Executor) option {
	return func(_ *engine.Config, d *engine.Deps) { d.Executor = e }
}

func withObserver(o engine.Observer) option {
	return func(_ *engine.Config, d *engine.Deps) { d.Observer = o }
}

// withStore wraps the store the ENGINE talks to and leaves h.store — the real
// one the test seeds and reads through — alone. A wrapper rather than a
// replacement, so an injected fault fires on the engine's writes and not on the
// harness's own setup, which is the same split the httpapi stream fixture uses.
func withStore(wrap func(engine.Store) engine.Store) option {
	return func(_ *engine.Config, d *engine.Deps) { d.Store = wrap(d.Store) }
}

func newHarness(t *testing.T, reg tools.Registry, opts ...option) *harness {
	t.Helper()

	h := &harness{
		t:     t,
		clk:   clock.NewFake(epoch),
		store: memstore.New(memstore.Options{}),
		reg:   reg,
	}
	// A generous buffer: the assertions read the stream as a queue, and a
	// dropped event would look like a step that never happened.
	h.events, h.unsub = h.store.Subscribe(1024)
	t.Cleanup(h.unsub)
	t.Cleanup(func() { _ = h.store.Close() })

	cfg := engine.Config{
		Owner:   "test",
		Workers: 1, ClaimBatch: 1,
		// Poll rarely: claiming is driven by the store's readiness hint, so a
		// short poll interval would only make Advance fire hundreds of
		// irrelevant ticks.
		PollInterval: time.Second,
		// Generous lease and abandon values so that a test advancing the clock
		// to fire one timer does not accidentally fire another: a step
		// deadline and a lease expiry landing on the same instant would make
		// the outcome depend on firing order rather than on the behaviour
		// under test.
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: time.Second,
		StoreTimeout:      time.Second,
		AbandonGrace:      5 * time.Second,
		// The reconciler is driven explicitly by the tests that want it.
		ReconcileInterval: time.Hour,
		ReconcileBatch:    100,
		Backoff:           engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2},
	}
	deps := engine.Deps{
		Store:    h.store,
		Executor: tools.Local{Registry: reg, MaxOutputBytes: 1 << 20, Log: quietLogger()},
		Clock:    h.clk,
		Log:      quietLogger(),
	}
	for _, o := range opts {
		o(&cfg, &deps)
	}

	eng, err := engine.New(cfg, deps)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.eng = eng
	return h
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// submit persists a job built from the given steps, stamped with the fake
// clock so the claim predicate lines up with whatever the test advances to.
func (h *harness) submit(id string, steps []runmesh.PlanStep, policy runmesh.FailurePolicy) *runmesh.Job {
	h.t.Helper()
	return h.submitAt(id, steps, policy, h.clk.Now())
}

// submitAt is for the few tests that drive an engine on the system clock,
// where a job stamped with the fake epoch might not be claimable yet.
func (h *harness) submitAt(id string, steps []runmesh.PlanStep, policy runmesh.FailurePolicy, at time.Time) *runmesh.Job {
	h.t.Helper()
	p := &runmesh.Plan{Name: id, OnStepFailure: policy, Steps: steps}
	j := p.Build(id, at, runmesh.Defaults{StepTimeout: 30 * time.Second, MaxAttempts: 3})
	out, err := h.store.CreateJob(h.t.Context(), j, "")
	if err != nil {
		h.t.Fatalf("CreateJob(%s): %v", id, err)
	}
	return out
}

// await consumes the event stream until pred matches.
//
// The event stream is both the assertion and the synchronisation primitive:
// tests never sleep and never poll, so a step that produces one spurious extra
// attempt fails here rather than passing a final-status check.
func (h *harness) await(pred func(runmesh.Event) bool, what string) runmesh.Event {
	h.t.Helper()

	for {
		// Scan everything received so far, skipping events an earlier await
		// already claimed, and claim exactly one match.
		h.seenMu.Lock()
		for i, e := range h.seen {
			if !h.used[i] && pred(e) {
				h.used[i] = true
				h.seenMu.Unlock()
				return e
			}
		}
		h.seenMu.Unlock()

		select {
		case e, ok := <-h.events:
			if !ok {
				h.t.Fatalf("the event stream closed before %s", what)
			}
			h.seenMu.Lock()
			h.seen = append(h.seen, e)
			h.used = append(h.used, false)
			h.seenMu.Unlock()
		case <-h.t.Context().Done():
			h.t.Fatalf("timed out waiting for %s; saw %v", what, h.typesSoFar())
			return runmesh.Event{}
		}
	}
}

func (h *harness) typesSoFar() []runmesh.EventType {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	out := make([]runmesh.EventType, 0, len(h.seen))
	for _, e := range h.seen {
		out = append(out, e.Type)
	}
	return out
}

func stepEvent(t runmesh.EventType, stepID string) func(runmesh.Event) bool {
	return func(e runmesh.Event) bool { return e.Type == t && e.StepID == stepID }
}

func jobFinished(e runmesh.Event) bool { return e.Type == runmesh.JobFinished }

func (h *harness) job(id string) *runmesh.Job {
	h.t.Helper()
	j, err := h.store.Job(h.t.Context(), id)
	if err != nil {
		h.t.Fatalf("Job(%s): %v", id, err)
	}
	return j
}

// drainAll runs steps synchronously on the calling goroutine until none are
// claimable. No goroutines, no clock, no possibility of a timing flake.
func (h *harness) drainAll() int {
	h.t.Helper()
	n := 0
	for range 100 {
		ran, err := h.eng.RunOnce(h.t.Context())
		if err != nil {
			h.t.Fatalf("RunOnce: %v", err)
		}
		if !ran {
			return n
		}
		n++
	}
	h.t.Fatal("RunOnce did not converge after 100 iterations")
	return n
}

// -------------------------------------------------------------------- tools

func echoStep(id string, deps ...string) runmesh.PlanStep {
	return runmesh.PlanStep{ID: id, Tool: "echo", DependsOn: deps}
}

func toolStep(id, tool string, params string, deps ...string) runmesh.PlanStep {
	s := runmesh.PlanStep{ID: id, Tool: tool, DependsOn: deps}
	if params != "" {
		s.Params = json.RawMessage(params)
	}
	return s
}

// gate blocks inside Run until it is released or its context ends. It is how
// a step is held in RUNNING while the test does something to it.
type gate struct {
	entered   chan struct{}
	release   chan struct{}
	ignoreCtx bool
	calls     atomic.Int32
}

func newGate(ignoreCtx bool) *gate {
	return &gate{
		entered:   make(chan struct{}, 64),
		release:   make(chan struct{}),
		ignoreCtx: ignoreCtx,
	}
}

func (g *gate) Run(ctx context.Context, in tools.Input) (tools.Output, error) {
	g.calls.Add(1)
	g.entered <- struct{}{}
	if g.ignoreCtx {
		<-g.release // deliberately ignores cancellation
		return tools.Output{Result: json.RawMessage(`{"late":true}`)}, nil
	}
	select {
	case <-g.release:
		return tools.Output{Result: json.RawMessage(`{"ok":true}`)}, nil
	case <-ctx.Done():
		return tools.Output{}, ctx.Err()
	}
}

func (g *gate) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-t.Context().Done():
		t.Fatal("the gated tool was never entered")
	}
}

// counter records how many invocations overlap, so concurrency is asserted by
// measurement rather than inferred from timing.
type counter struct {
	live    atomic.Int32
	max     atomic.Int32
	entered chan struct{}
	hold    chan struct{}
}

func newCounter() *counter {
	return &counter{entered: make(chan struct{}, 64), hold: make(chan struct{})}
}

func (c *counter) Run(ctx context.Context, in tools.Input) (tools.Output, error) {
	n := c.live.Add(1)
	for {
		peak := c.max.Load()
		if n <= peak || c.max.CompareAndSwap(peak, n) {
			break
		}
	}
	defer c.live.Add(-1)
	// Signalled AFTER the gauge is updated, so a test that waits for N entries
	// is guaranteed to observe the peak they produced.
	c.entered <- struct{}{}
	select {
	case <-c.hold:
	case <-ctx.Done():
		return tools.Output{}, ctx.Err()
	}
	return tools.Output{Result: json.RawMessage(`{"ok":true}`)}, nil
}

func (c *counter) waitEntered(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-c.entered:
		case <-t.Context().Done():
			t.Fatalf("only %d of %d invocations started", c.live.Load(), n)
		}
	}
}

// flaky fails a configurable number of attempts before succeeding.
type flaky struct {
	failUntil int
	kind      string
}

func (f flaky) Run(ctx context.Context, in tools.Input) (tools.Output, error) {
	if in.Attempt > f.failUntil {
		return tools.Output{Result: json.RawMessage(`{"ok":true}`)}, nil
	}
	switch f.kind {
	case "fatal":
		return tools.Output{}, runmesh.Fatal("bad_input", "attempt %d", in.Attempt)
	case "panic":
		panic("injected tool panic")
	case "unclassified":
		return tools.Output{}, errors.New("something went wrong and nobody said what kind")
	default:
		return tools.Output{}, runmesh.Retry("transient", "attempt %d", in.Attempt)
	}
}

// -------------------------------------------------------------- synchronous

func TestHappyPathTimeline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	h.submit("job_a", []runmesh.PlanStep{echoStep("only")}, runmesh.FailFast)

	if n := h.drainAll(); n != 1 {
		t.Fatalf("ran %d steps, want 1", n)
	}

	page, err := h.store.JobEvents(t.Context(), "job_a", 0, 100)
	if err != nil {
		t.Fatalf("JobEvents: %v", err)
	}
	want := []runmesh.EventType{
		runmesh.JobCreated, runmesh.StepScheduled, runmesh.JobStarted,
		runmesh.StepStarted, runmesh.StepFinished, runmesh.JobFinished,
	}
	got := make([]runmesh.EventType, len(page.Events))
	for i, e := range page.Events {
		got[i] = e.Type
	}
	if len(got) != len(want) {
		t.Fatalf("timeline = %v,\n     want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("timeline = %v,\n     want %v", got, want)
		}
	}

	j := h.job("job_a")
	if j.State != runmesh.Succeeded {
		t.Errorf("job state = %s, want SUCCEEDED", j.State)
	}
	if s := j.Step("only"); s.Attempt != 1 || s.Failures != 0 || len(s.Result) == 0 {
		t.Errorf("step = attempt %d, failures %d, result %s", s.Attempt, s.Failures, s.Result)
	}
}

func TestDAGExecutesInDependencyOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	h.submit("job_d", []runmesh.PlanStep{
		echoStep("a"),
		echoStep("b", "a"),
		echoStep("c", "a"),
		echoStep("d", "b", "c"),
	}, runmesh.FailFast)

	if n := h.drainAll(); n != 4 {
		t.Fatalf("ran %d steps, want 4", n)
	}

	j := h.job("job_d")
	if j.State != runmesh.Succeeded {
		t.Fatalf("job state = %s, want SUCCEEDED", j.State)
	}
	// Every step must have started only after all its dependencies ended.
	for _, s := range j.Steps {
		for _, dep := range s.DependsOn {
			d := j.Step(dep)
			if d.EndedAt == nil || s.StartedAt == nil {
				t.Fatalf("missing timestamps on %s or %s", s.ID, dep)
			}
			if s.StartedAt.Before(*d.EndedAt) {
				t.Errorf("step %s started at %v, before its dependency %s ended at %v",
					s.ID, s.StartedAt, dep, d.EndedAt)
			}
		}
	}
	// The dependency's output reached the dependent.
	var result struct {
		Deps map[string]json.RawMessage `json:"deps"`
	}
	if err := json.Unmarshal(j.Step("d").Result, &result); err != nil {
		t.Fatalf("decode d's result: %v", err)
	}
	if len(result.Deps) != 2 {
		t.Errorf("step d saw %d dependency outputs, want 2", len(result.Deps))
	}
}

func TestRetryLadder(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"flaky": flaky{failUntil: 2}},
		withBackoff(engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}))
	h.submit("job_r", []runmesh.PlanStep{
		{ID: "s", Tool: "flaky", MaxAttempts: 3},
	}, runmesh.FailFast)

	// Attempt 1 fails; the step is not claimable again until the backoff has
	// elapsed, which is entirely expressed as next_attempt_at.
	if !h.mustRunOnce() {
		t.Fatal("attempt 1 did not run")
	}
	if ran, _ := h.eng.RunOnce(t.Context()); ran {
		t.Fatal("the step was claimable before its backoff elapsed")
	}
	s := h.job("job_r").Step("s")
	if s.State != runmesh.Retrying || s.Failures != 1 || s.Attempt != 1 {
		t.Fatalf("after attempt 1: state=%s attempt=%d failures=%d", s.State, s.Attempt, s.Failures)
	}
	wantNext := epoch.Add(time.Second)
	if !s.NextAttemptAt.Equal(wantNext) {
		t.Errorf("next_attempt_at = %v, want %v (base delay)", s.NextAttemptAt, wantNext)
	}

	// Attempt 2, one second later.
	h.clk.Advance(time.Second)
	if !h.mustRunOnce() {
		t.Fatal("attempt 2 did not run once the backoff elapsed")
	}
	s = h.job("job_r").Step("s")
	if s.Failures != 2 || s.Attempt != 2 {
		t.Fatalf("after attempt 2: attempt=%d failures=%d", s.Attempt, s.Failures)
	}
	if want := h.clk.Now().Add(2 * time.Second); !s.NextAttemptAt.Equal(want) {
		t.Errorf("next_attempt_at = %v, want %v (the delay doubles)", s.NextAttemptAt, want)
	}

	// Attempt 3 succeeds.
	h.clk.Advance(2 * time.Second)
	if !h.mustRunOnce() {
		t.Fatal("attempt 3 did not run")
	}
	j := h.job("job_r")
	if j.State != runmesh.Succeeded {
		t.Fatalf("job state = %s, want SUCCEEDED", j.State)
	}
	s = j.Step("s")
	if s.Attempt != 3 || s.Failures != 2 {
		t.Errorf("final: attempt=%d failures=%d, want 3 and 2", s.Attempt, s.Failures)
	}

	// Exactly two retries were scheduled: a coarser assertion would not notice
	// a third, spurious attempt.
	page, _ := h.store.JobEvents(t.Context(), "job_r", 0, 200)
	retries := 0
	for _, e := range page.Events {
		if e.Type == runmesh.StepRetryScheduled {
			retries++
		}
	}
	if retries != 2 {
		t.Errorf("%d retries were scheduled, want exactly 2", retries)
	}
}

func (h *harness) mustRunOnce() bool {
	h.t.Helper()
	ran, err := h.eng.RunOnce(h.t.Context())
	if err != nil {
		h.t.Fatalf("RunOnce: %v", err)
	}
	return ran
}

// TestTerminalErrorIsNotRetried is the counterpart to the ladder above. A
// coarser assertion ("the job failed") would pass even if the runtime had
// retried three times first.
func TestTerminalErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"flaky": flaky{failUntil: 99, kind: "fatal"}})
	h.submit("job_f", []runmesh.PlanStep{{ID: "s", Tool: "flaky", MaxAttempts: 5}}, runmesh.FailFast)

	h.drainAll()

	j := h.job("job_f")
	if j.State != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED", j.State)
	}
	s := j.Step("s")
	if s.Attempt != 1 {
		t.Errorf("a terminal error was retried: attempt = %d, want 1", s.Attempt)
	}
	if s.Error == nil || s.Error.Code != "bad_input" {
		t.Errorf("error = %+v, want the tool's own code", s.Error)
	}
	page, _ := h.store.JobEvents(t.Context(), "job_f", 0, 200)
	for _, e := range page.Events {
		if e.Type == runmesh.StepRetryScheduled {
			t.Fatal("a retry was scheduled for a terminal error")
		}
	}
}

func TestUnclassifiedErrorIsTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"flaky": flaky{failUntil: 99, kind: "unclassified"}})
	h.submit("job_u", []runmesh.PlanStep{{ID: "s", Tool: "flaky", MaxAttempts: 5}}, runmesh.FailFast)
	h.drainAll()

	s := h.job("job_u").Step("s")
	if s.State != runmesh.Failed {
		t.Fatalf("state = %s, want FAILED: an unclassified error must not be retried", s.State)
	}
	if s.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", s.Attempt)
	}
	if s.Error == nil || s.Error.Code != runmesh.CodeUnclassified {
		t.Errorf("error = %+v, want code %q", s.Error, runmesh.CodeUnclassified)
	}
}

// TestPanicIsIsolated: a panicking tool becomes an ordinary terminal outcome,
// the process survives, and the engine goes on to run the next step.
func TestPanicIsIsolated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{
		"boom": flaky{failUntil: 99, kind: "panic"},
		"echo": tools.Echo{},
	})
	h.submit("job_p", []runmesh.PlanStep{{ID: "s", Tool: "boom", MaxAttempts: 3}}, runmesh.FailFast)
	h.submit("job_ok", []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)

	h.drainAll()

	s := h.job("job_p").Step("s")
	if s.State != runmesh.Failed {
		t.Errorf("state = %s, want FAILED", s.State)
	}
	if s.Error == nil || s.Error.Code != runmesh.CodePanic {
		t.Errorf("error = %+v, want code %q", s.Error, runmesh.CodePanic)
	}
	if s.Attempt != 1 {
		t.Errorf("a panic was retried: attempt = %d", s.Attempt)
	}
	if got := h.job("job_ok").State; got != runmesh.Succeeded {
		t.Errorf("the next job did not run after a panic: state = %s", got)
	}
}

func TestFailFastCancelsSiblings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{
		"bad":  flaky{failUntil: 99, kind: "fatal"},
		"echo": tools.Echo{},
	})
	h.submit("job_ff", []runmesh.PlanStep{
		{ID: "bad", Tool: "bad", MaxAttempts: 1},
		echoStep("sib"),
		echoStep("after", "bad"),
	}, runmesh.FailFast)

	h.drainAll()

	j := h.job("job_ff")
	if j.State != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED", j.State)
	}
	if j.CancelReason != runmesh.CancelStepFailed {
		t.Errorf("cancel reason = %q, want %q", j.CancelReason, runmesh.CancelStepFailed)
	}
	for _, id := range []string{"sib", "after"} {
		if got := j.Step(id).State; got != runmesh.Cancelled {
			t.Errorf("step %s = %s under fail_fast, want CANCELLED", id, got)
		}
	}
}

func TestContinueOnFailureRunsWhatItCan(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{
		"bad":  flaky{failUntil: 99, kind: "fatal"},
		"echo": tools.Echo{},
	})
	h.submit("job_cof", []runmesh.PlanStep{
		{ID: "bad", Tool: "bad", MaxAttempts: 1},
		echoStep("independent"),
		echoStep("downstream", "bad"),
	}, runmesh.ContinueOnFailure)

	h.drainAll()

	j := h.job("job_cof")
	if j.State != runmesh.Failed {
		t.Fatalf("job state = %s, want FAILED", j.State)
	}
	if got := j.Step("independent").State; got != runmesh.Succeeded {
		t.Errorf("the independent step = %s; continue_on_failure must still run it", got)
	}
	ds := j.Step("downstream")
	if ds.State != runmesh.Cancelled {
		t.Errorf("the doomed dependent = %s, want CANCELLED", ds.State)
	}
	if ds.Error == nil || ds.Error.Code != runmesh.CodeDepFailed {
		t.Errorf("the doomed dependent's error = %+v, want code %q", ds.Error, runmesh.CodeDepFailed)
	}
}

// ---------------------------------------------------------------- concurrent

// TestWorkerPoolRunsStepsConcurrently measures overlap with an atomic gauge
// rather than inferring it from timing, so it cannot be flaky.
func TestWorkerPoolRunsStepsConcurrently(t *testing.T) {
	t.Parallel()
	c := newCounter()
	h := newHarness(t, tools.Registry{"count": c}, withWorkers(4))

	h.submit("job_c", []runmesh.PlanStep{
		toolStep("a", "count", ""),
		toolStep("b", "count", ""),
		toolStep("c", "count", ""),
		toolStep("d", "count", ""),
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until all four tools are actually inside Run — not merely until
	// STEP_STARTED was written, which happens before the tool is invoked —
	// then let them all finish.
	c.waitEntered(t, 4)
	if got := c.max.Load(); got != 4 {
		t.Errorf("peak concurrency = %d, want 4: four independent steps and four workers", got)
	}
	close(c.hold)

	h.await(jobFinished, "the job to finish")
	if got := h.job("job_c").State; got != runmesh.Succeeded {
		t.Fatalf("job state = %s, want SUCCEEDED", got)
	}

	if err := h.eng.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := h.eng.Inflight(); n != 0 {
		t.Errorf("%d steps still in flight after shutdown", n)
	}
}

// TestStepTimeout drives a real per-step deadline entirely from the fake
// clock: no sleeps, and the whole test finishes in microseconds.
func TestStepTimeout(t *testing.T) {
	t.Parallel()
	g := newGate(false)
	h := newHarness(t, tools.Registry{"gate": g})
	h.submit("job_t", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 1, MaxAttempts: 2},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	// dispatcher ticker + reconciler ticker + the step's deadline + its
	// heartbeat ticker.
	if err := h.clk.BlockUntilContext(t.Context(), 4); err != nil {
		t.Fatalf("waiting for the step to park: %v", err)
	}
	h.clk.Advance(time.Second)

	e := h.await(stepEvent(runmesh.StepRetryScheduled, "s"), "a retry to be scheduled")
	if e.Error == nil || e.Error.Code != runmesh.CodeTimeout {
		t.Fatalf("the retry's error = %+v, want code %q — a timeout must not be reported as unclassified", e.Error, runmesh.CodeTimeout)
	}
	s := h.job("job_t").Step("s")
	if s.State != runmesh.Retrying {
		t.Errorf("state = %s, want RETRYING: timeouts are retryable", s.State)
	}
	if s.Failures != 1 {
		t.Errorf("failures = %d, want 1", s.Failures)
	}

	close(g.release)
	_ = h.eng.Shutdown(t.Context())
}

// TestCancelInFlight is the regression test for the hardest interaction in the
// design: a cancellation arriving while a step is running must produce
// CANCELLED — not RETRYING, not FAILED — and must spend no retry budget.
func TestCancelInFlight(t *testing.T) {
	t.Parallel()
	g := newGate(false)
	h := newHarness(t, tools.Registry{"gate": g})
	h.submit("job_x", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 600, MaxAttempts: 3},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	if _, err := h.store.RequestCancel(t.Context(), "job_x", runmesh.CancelUser, h.clk.Now()); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if err := h.clk.BlockUntilContext(t.Context(), 4); err != nil {
		t.Fatalf("waiting for the heartbeat to arm: %v", err)
	}
	// One heartbeat interval: the running step learns it should stop through
	// the same round trip that renews its lease.
	h.clk.Advance(time.Second)

	h.await(jobFinished, "the job to finish")

	j := h.job("job_x")
	if j.State != runmesh.Cancelled {
		t.Fatalf("job state = %s, want CANCELLED", j.State)
	}
	s := j.Step("s")
	if s.State != runmesh.Cancelled {
		t.Errorf("step state = %s, want CANCELLED", s.State)
	}
	if s.Failures != 0 {
		t.Errorf("cancellation spent %d units of retry budget; it must spend none", s.Failures)
	}
	if s.EndedAt == nil {
		t.Error("the cancelled step has no end timestamp; the outcome write inherited a dead context")
	}
	page, _ := h.store.JobEvents(t.Context(), "job_x", 0, 200)
	for _, e := range page.Events {
		if e.Type == runmesh.StepRetryScheduled {
			t.Fatal("a cancelled step was scheduled for retry")
		}
	}

	close(g.release)
	_ = h.eng.Shutdown(t.Context())
}

// TestAbandonedToolDoesNotParkAWorker: a tool that ignores its context must
// not be able to hold a worker for ever. After the abandon grace the step is
// settled without it and the worker returns its token.
func TestAbandonedToolDoesNotParkAWorker(t *testing.T) {
	t.Parallel()
	g := newGate(true) // deliberately ignores cancellation
	h := newHarness(t, tools.Registry{"gate": g, "echo": tools.Echo{}})
	h.submit("job_hang", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 1, MaxAttempts: 1},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	if err := h.clk.BlockUntilContext(t.Context(), 4); err != nil {
		t.Fatalf("waiting for the step to park: %v", err)
	}
	h.clk.Advance(time.Second) // the step deadline fires and arms the abandon grace
	if err := h.clk.BlockUntilContext(t.Context(), 4); err != nil {
		t.Fatalf("waiting for the abandon grace to arm: %v", err)
	}
	h.clk.Advance(6 * time.Second) // past the abandon grace

	h.await(jobFinished, "the abandoned step to be settled without its tool")

	s := h.job("job_hang").Step("s")
	if s.State != runmesh.TimedOut {
		t.Errorf("state = %s, want TIMED_OUT", s.State)
	}

	// The worker took its token back, so the pool still runs new work.
	h.submit("job_next", []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)
	h.await(func(e runmesh.Event) bool {
		return e.JobID == "job_next" && e.Type == runmesh.JobFinished
	}, "the next job to run on the recovered worker")

	close(g.release)
}

// TestReconcilerReclaimsExpiredLeases is crash recovery: a worker that dies
// mid-step leaves a lease nobody will renew, and the reconciler must put that
// step back rather than leaving the job stuck in RUNNING for ever.
func TestReconcilerReclaimsExpiredLeases(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	h.submit("job_dead", []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)

	// Claim without ever settling: exactly what a killed worker leaves behind.
	leases, err := h.store.Claim(t.Context(), runmesh.ClaimRequest{
		Owner: "dead-worker", Limit: 1, LeaseTTL: time.Second, Now: h.clk.Now(),
	})
	if err != nil || len(leases) != 1 {
		t.Fatalf("Claim: %v (%d leases)", err, len(leases))
	}

	h.clk.Advance(2 * time.Second)
	if n := h.eng.Reconcile(t.Context()); n != 1 {
		t.Fatalf("the sweep reclaimed %d leases, want 1", n)
	}

	s := h.job("job_dead").Step("s")
	if s.State != runmesh.Queued {
		t.Fatalf("state = %s, want QUEUED", s.State)
	}
	if s.Failures != 1 {
		t.Errorf("failures = %d, want 1: an expired lease spends budget, unlike a graceful release", s.Failures)
	}
	if s.LeaseID != "" {
		t.Error("the expired lease was not cleared")
	}

	// And it runs to completion on the next attempt.
	h.drainAll()
	if got := h.job("job_dead").State; got != runmesh.Succeeded {
		t.Errorf("job state = %s, want SUCCEEDED after recovery", got)
	}
}

// TestZombieWorkerCannotOverwrite: the worker whose lease was reclaimed must
// write nothing at all, or it would clobber the new holder's result.
func TestZombieWorkerCannotOverwrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	h.submit("job_z", []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)

	stale, err := h.store.Claim(t.Context(), runmesh.ClaimRequest{
		Owner: "zombie", Limit: 1, LeaseTTL: time.Second, Now: h.clk.Now(),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	h.clk.Advance(2 * time.Second)
	h.eng.Reconcile(t.Context())
	h.drainAll() // a fresh worker claims and completes it

	if got := h.job("job_z").Step("s").State; got != runmesh.Succeeded {
		t.Fatalf("state = %s, want SUCCEEDED", got)
	}

	// The zombie now tries to report failure with its stale token.
	err = h.store.Finish(t.Context(), runmesh.Outcome{
		Lease: stale[0], State: runmesh.Failed, CountFail: true,
		Error:   &runmesh.ErrorInfo{Code: "zombie"},
		EndedAt: h.clk.Now(),
	})
	if !errors.Is(err, runmesh.ErrLeaseLost) {
		t.Fatalf("the zombie's write returned %v, want ErrLeaseLost", err)
	}
	if got := h.job("job_z").Step("s").State; got != runmesh.Succeeded {
		t.Fatalf("the zombie overwrote the result: state = %s", got)
	}
}

// ------------------------------------------------------------------ shutdown

func TestDrainCleanly(t *testing.T) {
	t.Parallel()
	g := newGate(false)
	h := newHarness(t, tools.Registry{"gate": g}, withWorkers(2))
	h.submit("job_drain", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 600, MaxAttempts: 3},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	// Shutdown must let in-flight work finish, so releasing the tool while the
	// drain is under way has to produce a clean result.
	done := make(chan error, 1)
	go func() { done <- h.eng.Shutdown(t.Context()) }()
	close(g.release)

	if err := <-done; err != nil {
		t.Fatalf("a clean drain returned %v, want nil", err)
	}
	if got := h.job("job_drain").Step("s").State; got != runmesh.Succeeded {
		t.Errorf("the in-flight step = %s; a clean drain must let it finish", got)
	}
}

// TestDrainDeadlineReleasesWithoutSpendingBudget: when the deadline expires,
// in-flight steps go back to QUEUED with their retry budget UNTOUCHED. A
// rolling restart must cost zero retries.
func TestDrainDeadlineReleasesWithoutSpendingBudget(t *testing.T) {
	t.Parallel()
	g := newGate(false)
	h := newHarness(t, tools.Registry{"gate": g})
	h.submit("job_dd", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 600, MaxAttempts: 3},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	// A context that is already done stands in for a drain deadline that has
	// expired, with no wall-clock waiting.
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.eng.Shutdown(expired); err == nil {
		t.Error("Shutdown returned nil after its deadline expired; the caller must be told")
	}

	h.await(stepEvent(runmesh.StepReleased, "s"), "the step to be released")

	s := h.job("job_dd").Step("s")
	if s.State != runmesh.Queued {
		t.Errorf("state = %s, want QUEUED", s.State)
	}
	if s.Failures != 0 {
		t.Errorf("the drain spent %d units of retry budget; a deploy must cost none", s.Failures)
	}
	if s.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 — monotonic, and not decremented by a release", s.Attempt)
	}

	close(g.release)
}

// TestShutdownIsIdempotent: every caller gets the same real answer, never a
// bare nil from the second one.
func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = h.eng.Shutdown(t.Context())
		}()
	}
	wg.Wait()
	for i, err := range results {
		if err != results[0] {
			t.Errorf("caller %d got %v, caller 0 got %v; every caller must see the same result",
				i, err, results[0])
		}
	}
	if results[0] != nil {
		t.Errorf("a clean shutdown returned %v", results[0])
	}
}

func TestShutdownBeforeStartIsANoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	if err := h.eng.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown before Start = %v, want nil", err)
	}
}

func TestStartTwiceIsRejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}})
	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := h.eng.Start(t.Context()); err == nil {
		t.Error("a second Start succeeded; it must be rejected rather than doubling the pool")
	}
	_ = h.eng.Shutdown(t.Context())
}

// ----------------------------------------------------------- fault injection

// faultStore makes Claim fail, and then misbehave, so the dispatcher's
// capacity accounting can be tested against a store that does not honour its
// own contract.
type faultStore struct {
	engine.Store
	mu          sync.Mutex
	failNext    int
	oversubscri bool
}

func (f *faultStore) Claim(ctx context.Context, req runmesh.ClaimRequest) ([]runmesh.Lease, error) {
	f.mu.Lock()
	fail := f.failNext > 0
	if fail {
		f.failNext--
	}
	over := f.oversubscri
	f.mu.Unlock()

	if fail {
		return nil, errors.New("injected claim failure")
	}
	if over {
		// Ask for more than requested, so the returned slice can exceed Limit.
		big := req
		big.Limit = req.Limit + 4
		return f.Store.Claim(ctx, big)
	}
	return f.Store.Claim(ctx, req)
}

// TestDispatcherSurvivesAMisbehavingStore: a failed claim must return every
// token it was holding, and an oversubscribing store must not be able to hand
// the pool more work than it has workers. Either bug shrinks capacity silently
// until the runtime stops making progress.
func TestDispatcherSurvivesAMisbehavingStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withWorkers(2))

	fs := &faultStore{Store: h.store, failNext: 3, oversubscri: true}
	eng, err := engine.New(engine.Config{
		Owner: "test", Workers: 2, ClaimBatch: 2,
		PollInterval:      10 * time.Millisecond,
		LeaseTTL:          time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		StoreTimeout:      time.Second,
		AbandonGrace:      200 * time.Millisecond,
		ReconcileInterval: time.Hour,
		ReconcileBatch:    100,
	}, engine.Deps{
		Store:    fs,
		Executor: tools.Local{Registry: h.reg, MaxOutputBytes: 1 << 20, Log: quietLogger()},
		Clock:    clock.System(), // real time: this test is about capacity, not deadlines
		Log:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	now := clock.System().Now()
	for i := range 6 {
		h.submitAt("job_"+string(rune('a'+i)), []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast, now)
	}
	if err := eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Shutdown(t.Context()) })

	// Every job must still complete: if the failed claims leaked their tokens,
	// the pool would shrink to zero and this would hang.
	finished := 0
	for finished < 6 {
		h.await(jobFinished, "all six jobs to finish")
		finished++
	}
	if got := eng.Stats().ClaimErrors; got < 3 {
		t.Errorf("claim errors = %d, want at least the 3 that were injected", got)
	}
}

// TestChaos runs a wide DAG through the real pool with cancellations
// interleaved, under -race, and asserts the one invariant that must hold no
// matter what: every job reaches a terminal state.
func TestChaos(t *testing.T) {
	t.Parallel()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withWorkers(8))

	// Six levels, each depending on the one above it.
	const jobs, levels, width = 12, 6, 3
	ids := make([]string, 0, jobs)
	for j := range jobs {
		id := "job_" + itoa(j)
		ids = append(ids, id)

		var steps []runmesh.PlanStep
		var prev []string
		for l := range levels {
			var level []string
			for w := range width {
				sid := "l" + itoa(l) + "s" + itoa(w)
				steps = append(steps, echoStep(sid, prev...))
				level = append(level, sid)
			}
			prev = level
		}
		policy := runmesh.FailFast
		if j%2 == 0 {
			policy = runmesh.ContinueOnFailure
		}
		h.submit(id, steps, policy)
	}

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Cancel a third of them, at whatever point they happen to have reached.
	go func() {
		for i, id := range ids {
			if i%3 != 0 {
				continue
			}
			_, _ = h.store.RequestCancel(context.Background(), id, runmesh.CancelUser, h.clk.Now())
		}
	}()

	for range jobs {
		h.await(jobFinished, "every job to reach a terminal state")
	}
	if err := h.eng.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	for _, id := range ids {
		j := h.job(id)
		if !j.State.Terminal() {
			t.Errorf("job %s ended in %s, which is not terminal", id, j.State)
		}
		for _, s := range j.Steps {
			if !s.State.Terminal() {
				t.Errorf("job %s step %s ended in %s, which is not terminal", id, s.ID, s.State)
			}
			if s.LeaseID != "" {
				t.Errorf("job %s step %s still holds a lease", id, s.ID)
			}
		}
	}
	if n := h.eng.Inflight(); n != 0 {
		t.Errorf("%d steps still in flight after shutdown", n)
	}
}

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

// ---------------------------------------------------- review regressions

// heartbeatSpy counts heartbeats so a test can wait until the worker has
// actually CONSUMED a tick.
//
// Advancing a fake clock only delivers the tick; the worker processes it on its
// own goroutine afterwards. Waiting on the waiter count cannot see the
// difference here, because the count is the same before and after (the step
// deadline is replaced by the abandon timer). Waiting for heartbeat N+1 to be
// entered is what proves heartbeat N was fully processed, since both run on the
// same goroutine in order.
type heartbeatSpy struct {
	engine.Store
	entered chan struct{}
}

func (h *heartbeatSpy) Heartbeat(ctx context.Context, l runmesh.Lease, now, until time.Time) (runmesh.Directive, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	return h.Store.Heartbeat(ctx, l, now, until)
}

func (h *heartbeatSpy) waitBeats(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-h.entered:
		case <-t.Context().Done():
			t.Fatalf("only saw fewer than %d heartbeats", n)
		}
	}
}

// TestAbandonTimerIsNotRearmedByHeartbeats is the regression for the worst bug
// an adversarial review of Week 1 found.
//
// The cancel flag is sticky, so every heartbeat after a cancellation reports the
// same cancellation. The abandon timer was re-armed on each of those, and with
// the shipped defaults (heartbeat 5s, grace 10s) it was therefore reset five
// seconds before it could ever fire. A tool ignoring its context held a worker
// for the life of the process, the job never terminalised, and every shutdown
// ended in ErrDrainIncomplete.
func TestAbandonTimerIsNotRearmedByHeartbeats(t *testing.T) {
	t.Parallel()
	g := newGate(true) // deliberately ignores cancellation
	h := newHarness(t, tools.Registry{"gate": g, "echo": tools.Echo{}})

	spy := &heartbeatSpy{Store: h.store, entered: make(chan struct{}, 64)}
	eng, err := engine.New(engine.Config{
		Owner: "test", Workers: 1, ClaimBatch: 1,
		PollInterval:      time.Hour, // only the readiness hint drives claiming here
		LeaseTTL:          30 * time.Second,
		HeartbeatInterval: time.Second,
		StoreTimeout:      time.Second,
		AbandonGrace:      5 * time.Second,
		ReconcileInterval: time.Hour,
		ReconcileBatch:    100,
	}, engine.Deps{
		Store:    spy,
		Executor: tools.Local{Registry: h.reg, MaxOutputBytes: 1 << 20, Log: quietLogger()},
		Clock:    h.clk,
		Log:      quietLogger(),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	h.submit("job_stuck", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 600, MaxAttempts: 1},
	}, runmesh.FailFast)

	if err := eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	// reconciler ticker + step deadline + heartbeat ticker.
	if err := h.clk.BlockUntilContext(t.Context(), 3); err != nil {
		t.Fatalf("waiting for the step to park: %v", err)
	}
	if _, err := h.store.RequestCancel(t.Context(), "job_stuck", runmesh.CancelUser, h.clk.Now()); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	// t=1s: the first heartbeat delivers the cancellation and arms the grace,
	// which is therefore due at t=6s.
	h.clk.Advance(time.Second)
	spy.waitBeats(t, 1)

	// t=2s: a second heartbeat. Waiting for it to be ENTERED proves the first
	// was fully processed, so the grace is armed and its deadline is fixed.
	// This is the heartbeat that used to push that deadline out.
	h.clk.Advance(time.Second)
	spy.waitBeats(t, 1)

	// Cross the grace. Several more heartbeats fire on the way, and none of
	// them may move the deadline.
	h.clk.Advance(5 * time.Second)

	h.await(jobFinished, "the abandoned step to settle despite repeated heartbeats")

	j := h.job("job_stuck")
	if !j.State.Terminal() {
		t.Fatalf("job state = %s; a tool ignoring its context must not hold a job open", j.State)
	}
	if got := j.Step("s").State; got != runmesh.Cancelled {
		t.Errorf("step state = %s, want CANCELLED", got)
	}
	if got := j.Step("s").Failures; got != 0 {
		t.Errorf("abandoning a cancelled step spent %d units of retry budget, want 0", got)
	}

	close(g.release)
	_ = eng.Shutdown(t.Context())
}

// TestTimeoutIsNotMisreadAsAContractViolation is the regression for a select
// race: when the step deadline fires and the tool returns at the same instant,
// Go picks a ready case at random. If it took the result first, `stop` was
// still StopNone and the tool's own context error was classified as
// tool_broke_contract — a terminal failure for what was really a timeout, and
// so never retried.
//
// It is asserted through Classify directly, because reproducing the race
// itself would be exactly the flaky test this codebase avoids: the fix is that
// the worker resolves the stop reason from the context state before consuming
// the result, and Classify's behaviour for each resolved reason is what has to
// hold.
func TestTimeoutIsNotMisreadAsAContractViolation(t *testing.T) {
	t.Parallel()
	b := engine.Backoff{Base: time.Second, Max: time.Minute, Factor: 2}

	// What the worker used to do: consume the result with stop unresolved.
	unresolved := engine.Classify(engine.StopNone, context.DeadlineExceeded, 0, 3, b)
	if unresolved.State != runmesh.Failed || unresolved.Error.Code != runmesh.CodeContractBroken {
		t.Fatalf("premise changed: an unresolved deadline now classifies as %+v", unresolved)
	}

	// What it does now: resolve the reason first, then classify.
	resolved := engine.Classify(engine.StopTimeout, context.DeadlineExceeded, 0, 3, b)
	if resolved.State != runmesh.Retrying {
		t.Errorf("a resolved timeout = %s, want RETRYING", resolved.State)
	}
	if resolved.Error.Code != runmesh.CodeTimeout {
		t.Errorf("a resolved timeout reports %q, want %q", resolved.Error.Code, runmesh.CodeTimeout)
	}

	// The same applies to a drain: it must release, not fail.
	drained := engine.Classify(engine.StopShutdown, context.Canceled, 0, 3, b)
	if !drained.Release || drained.CountFail {
		t.Errorf("a resolved drain = %+v, want a release that spends no budget", drained)
	}
}

// ------------------------------------------------- the observer seam, Week 6

// recordingObserver counts exactly what internal/metrics counts, using the same
// labels, so an assertion made here is an assertion about the exporter.
//
// It is a mutex over maps rather than atomics, which is precisely what the
// Observer doc forbids a PRODUCTION implementation from being — but a test
// observer runs in a test, and being able to read a map back by label is worth
// more here than being fast. The shipped implementation is held to the real
// contract by TestObserveIsAllocationFree in internal/metrics.
type recordingObserver struct {
	mu sync.Mutex

	claims      int
	claimErrors int
	requested   int
	returned    int
	dispatched  int
	wokeOn      map[string]int
	started     int
	toolRuns    int
	settled     []engine.AttemptOutcome
	heartbeats  map[string]int
	reclaimed   map[runmesh.State]int
	sweeps      int
	sweepErrors int
	sweptTotal  int
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{
		wokeOn:     map[string]int{},
		heartbeats: map[string]int{},
		reclaimed:  map[runmesh.State]int{},
	}
}

func (o *recordingObserver) Claimed(requested, returned int, _ time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.claims++
	o.requested += requested
	o.returned += returned
	if err != nil {
		o.claimErrors++
	}
}

func (o *recordingObserver) Dispatched(string, time.Duration) {
	o.mu.Lock()
	o.dispatched++
	o.mu.Unlock()
}

func (o *recordingObserver) DispatcherIdle(on string) {
	o.mu.Lock()
	o.wokeOn[on]++
	o.mu.Unlock()
}

func (o *recordingObserver) AttemptStarted(string, time.Duration) {
	o.mu.Lock()
	o.started++
	o.mu.Unlock()
}

func (o *recordingObserver) ToolExecuted(string, time.Duration) {
	o.mu.Lock()
	o.toolRuns++
	o.mu.Unlock()
}

func (o *recordingObserver) AttemptSettled(out engine.AttemptOutcome) {
	o.mu.Lock()
	o.settled = append(o.settled, out)
	o.mu.Unlock()
}

func (o *recordingObserver) Heartbeat(_, outcome string) {
	o.mu.Lock()
	o.heartbeats[outcome]++
	o.mu.Unlock()
}

func (o *recordingObserver) LeaseReclaimed(s runmesh.State) {
	o.mu.Lock()
	o.reclaimed[s]++
	o.mu.Unlock()
}

func (o *recordingObserver) SweepFinished(n int, _ time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sweeps++
	o.sweptTotal += n
	if err != nil {
		o.sweepErrors++
	}
}

func (o *recordingObserver) totalReclaimed() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, v := range o.reclaimed {
		n += v
	}
	return n
}

func (o *recordingObserver) outcomes() []engine.AttemptOutcome {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]engine.AttemptOutcome(nil), o.settled...)
}

// TestLeasesExpiredMetricMatchesStats converts a drift RISK into an asserted
// invariant.
//
// e.leasesExpired.Add is at one place in Reconcile and the observer call is at
// another, a few lines apart, and both count the same event. They agree today.
// They will stop agreeing the first time somebody adds an early return between
// them — and nothing would fail, because a counter that is merely too low still
// looks like a counter. This is the test that fails instead.
func TestLeasesExpiredMetricMatchesStats(t *testing.T) {
	t.Parallel()

	obs := newRecordingObserver()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withObserver(obs))

	// Three steps, claimed by a worker that then dies: exactly what a killed
	// process leaves behind.
	for _, id := range []string{"job_a", "job_b", "job_c"} {
		h.submit(id, []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)
	}
	leases, err := h.store.Claim(t.Context(), runmesh.ClaimRequest{
		Owner: "dead-worker", Limit: 3, LeaseTTL: time.Second, Now: h.clk.Now(),
	})
	if err != nil || len(leases) != 3 {
		t.Fatalf("Claim: %v (%d leases)", err, len(leases))
	}

	h.clk.Advance(2 * time.Second)
	if n := h.eng.Reconcile(t.Context()); n != 3 {
		t.Fatalf("the sweep reclaimed %d leases, want 3", n)
	}
	// A second sweep with nothing to find: it must still be reported, or the
	// sweep rate would be indistinguishable from a reconciler that has stopped.
	h.eng.Reconcile(t.Context())

	stats := h.eng.Stats()
	if got, want := obs.totalReclaimed(), int(stats.LeasesExpired); got != want {
		t.Errorf("the observer saw %d reclaimed leases, Stats() reports %d; "+
			"the two counters have drifted", got, want)
	}
	if got := obs.reclaimed[runmesh.Queued]; got != 3 {
		t.Errorf("reclaimed{new_state=QUEUED} = %d, want 3", got)
	}
	if obs.sweeps != 2 {
		t.Errorf("sweeps = %d, want 2 including the one that found nothing", obs.sweeps)
	}
	if obs.sweepErrors != 0 {
		t.Errorf("sweep errors = %d, want 0", obs.sweepErrors)
	}
	if obs.sweptTotal != 3 {
		t.Errorf("the sweeps reported %d reclaimed leases in total, want 3", obs.sweptTotal)
	}
}

// TestClaimErrorsMetricMatchesStats is the same invariant on the dispatcher's
// side: e.claimErrors.Add and the observer both see the error from one Claim,
// and they are deliberately not the same increment.
func TestClaimErrorsMetricMatchesStats(t *testing.T) {
	t.Parallel()

	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withWorkers(2))
	obs := newRecordingObserver()
	fs := &faultStore{Store: h.store, failNext: 3}

	eng, err := engine.New(engine.Config{
		Owner: "test", Workers: 2, ClaimBatch: 2,
		PollInterval:      10 * time.Millisecond,
		LeaseTTL:          time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		StoreTimeout:      time.Second,
		AbandonGrace:      200 * time.Millisecond,
		ReconcileInterval: time.Hour,
		ReconcileBatch:    100,
	}, engine.Deps{
		Store:    fs,
		Executor: tools.Local{Registry: h.reg, MaxOutputBytes: 1 << 20, Log: quietLogger()},
		Observer: obs,
		// Real time, like TestDispatcherSurvivesAMisbehavingStore: this test is
		// about counter agreement under a live dispatcher, not about deadlines.
		Clock: clock.System(),
		Log:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	now := clock.System().Now()
	for i := range 4 {
		h.submitAt("job_"+string(rune('a'+i)), []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast, now)
	}
	if err := eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for range 4 {
		h.await(jobFinished, "all four jobs to finish")
	}

	// Shut down FIRST, so the dispatcher is stopped and the two counters can be
	// compared at an instant neither of them is moving. Reading them from a
	// running dispatcher would make this test flaky for a reason that has
	// nothing to do with the invariant it is asserting.
	if err := eng.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	stats := eng.Stats()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.claimErrors != int(stats.ClaimErrors) {
		t.Errorf("the observer saw %d claim errors, Stats() reports %d; "+
			"the two counters have drifted", obs.claimErrors, stats.ClaimErrors)
	}
	if obs.claimErrors < 3 {
		t.Errorf("claim errors = %d, want at least the 3 that were injected", obs.claimErrors)
	}
	if obs.returned > obs.requested {
		t.Errorf("the dispatcher was handed %d leases having asked for %d", obs.returned, obs.requested)
	}
	if obs.claims < obs.claimErrors {
		t.Errorf("%d claims reported but %d of them failed", obs.claims, obs.claimErrors)
	}
}

// TestEveryAttemptIsSettledExactlyOnce. settle has FIVE exit paths — the
// discard, the release, the two halves of a failed Finish, and the success —
// and every one of them reports, which is what stops
// runmesh_step_attempts_total from quietly under-counting the outcomes nobody
// watches.
//
// The subtests below are the three worth asserting here. The success path is
// the one every other test in this file drives, so it is the one a refactor is
// least likely to break; the other two are the ones that write NOTHING to the
// store, which means the counter is the only trace they leave and a missing
// report would be invisible. Of the two not covered here, the release is
// exercised by TestDrainDeadlineReleasesWithoutSpendingBudget (through its
// store-side effect rather than through this counter), and the reclaimed-Finish
// arm is the same branch as the failed-Finish one below with a different
// sentinel and a different log line.
func TestEveryAttemptIsSettledExactlyOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"a successful attempt", settledOnSuccess},
		{"an outcome discarded because a second owner took the step", settledOnADiscard},
		{"an outcome whose store write failed", settledWhenTheStoreWriteFails},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func settledOnSuccess(t *testing.T) {
	obs := newRecordingObserver()
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withObserver(obs))

	h.submit("job_ok", []runmesh.PlanStep{echoStep("a"), echoStep("b")}, runmesh.FailFast)
	h.drainAll()

	outcomes := obs.outcomes()
	if len(outcomes) != 2 {
		t.Fatalf("%d attempts settled, want 2", len(outcomes))
	}
	for _, o := range outcomes {
		if o.Tool != "echo" {
			t.Errorf("tool = %q, want echo", o.Tool)
		}
		if o.State != runmesh.Succeeded {
			t.Errorf("state = %s, want SUCCEEDED", o.State)
		}
		if o.Stop != engine.StopNone {
			t.Errorf("stop = %s, want none", o.Stop)
		}
		if o.Code != "" {
			t.Errorf("code = %q, want empty for a success", o.Code)
		}
		if o.Discarded || o.Released {
			t.Errorf("a successful attempt reports discarded=%v released=%v", o.Discarded, o.Released)
		}
		if o.Duration < 0 {
			t.Errorf("duration = %v, want a non-negative span", o.Duration)
		}
	}

	// The per-attempt seams fired too: every attempt that reached the tool was
	// started and executed.
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if obs.started != 2 {
		t.Errorf("attempts started = %d, want 2", obs.started)
	}
	if obs.toolRuns != 2 {
		t.Errorf("tool executions = %d, want 2", obs.toolRuns)
	}
	// And the DISPATCHER seams did not, because drainAll drives RunOnce, which
	// claims and executes on the calling goroutine without the dispatcher.
	// Asserting the zero is the point: it pins that Claimed and Dispatched
	// report the dispatcher's work and not the pool's, so a per-tool queue-wait
	// histogram is never fed by a synchronous single-step run that had no queue
	// to wait in.
	if obs.dispatched != 0 {
		t.Errorf("dispatched = %d, want 0: RunOnce bypasses the dispatcher", obs.dispatched)
	}
	if obs.claims != 0 {
		t.Errorf("claims = %d, want 0: RunOnce claims directly", obs.claims)
	}
}

// settledOnADiscard drives the one exit path that writes nothing at all.
//
// A second owner takes the step while the first attempt is still running; the
// first attempt discovers it on the round trip that would have renewed its
// lease, and settle then returns WITHOUT touching the store, because writing
// anything there is the zombie overwrite the fencing token exists to prevent.
// The counter is therefore the only evidence the attempt happened, and an
// unreported discard is a step_attempts_total that undercounts by one every
// time a lease is stolen.
func settledOnADiscard(t *testing.T) {
	obs := newRecordingObserver()
	g := newGate(false)
	h := newHarness(t, tools.Registry{"gate": g, "echo": tools.Echo{}}, withObserver(obs))
	h.submit("job_stolen", []runmesh.PlanStep{
		{ID: "s", Tool: "gate", TimeoutSec: 600, MaxAttempts: 3},
	}, runmesh.FailFast)

	if err := h.eng.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.await(stepEvent(runmesh.StepStarted, "s"), "the step to start")
	g.waitEntered(t)

	// THE STEAL IS DRIVEN THROUGH THE STORE, NOT THROUGH THE CLOCK. Advancing
	// the fake clock far enough for this lease to expire would fire the running
	// attempt's own heartbeat first and renew the very lease this is trying to
	// take. ExpireLeases and Claim both take their timestamp as a parameter —
	// the same property that lets them be SQL bind variables — so the sweep and
	// the second claim can happen at a notional minute from now while the
	// engine's clock has not moved at all.
	stealAt := h.clk.Now().Add(time.Minute)
	if _, err := h.store.ExpireLeases(t.Context(), stealAt, 10); err != nil {
		t.Fatalf("ExpireLeases: %v", err)
	}
	stolen, err := h.store.Claim(t.Context(), runmesh.ClaimRequest{
		Owner: "second-owner", Limit: 1, LeaseTTL: time.Minute, Now: stealAt,
	})
	if err != nil || len(stolen) != 1 {
		t.Fatalf("the second owner claimed %d steps: %v", len(stolen), err)
	}

	// dispatcher ticker + reconciler ticker + the step's deadline + its
	// heartbeat ticker.
	if err := h.clk.BlockUntilContext(t.Context(), 4); err != nil {
		t.Fatalf("waiting for the heartbeat to arm: %v", err)
	}
	h.clk.Advance(time.Second)

	// A discard leaves no event, so there is nothing on the stream to wait for.
	// What it does leave is a returned idle token, and with one worker a second
	// job cannot start until the discarded attempt has settled — so the second
	// job's completion IS the synchronisation point, on the event stream like
	// every other wait in this file.
	h.submit("job_after", []runmesh.PlanStep{echoStep("s")}, runmesh.FailFast)
	h.await(func(e runmesh.Event) bool {
		return e.JobID == "job_after" && e.Type == runmesh.JobFinished
	}, "the next job to run on the worker the discarded attempt released")

	var discards []engine.AttemptOutcome
	for _, o := range obs.outcomes() {
		if o.Tool == "gate" {
			discards = append(discards, o)
		}
	}
	if len(discards) != 1 {
		t.Fatalf("the stolen attempt reported %d outcomes, want exactly 1: %+v",
			len(discards), discards)
	}
	o := discards[0]
	if !o.Discarded {
		t.Errorf("discarded = false for an attempt whose lease was taken: %+v", o)
	}
	if o.Released {
		t.Errorf("released = true on a discard; a discard writes nothing, and a "+
			"release is a store call: %+v", o)
	}
	if o.Stop != engine.StopLost {
		t.Errorf("stop = %s, want %s", o.Stop, engine.StopLost)
	}
	if o.Reason != runmesh.CodeLeaseLost {
		t.Errorf("reason = %q, want %q", o.Reason, runmesh.CodeLeaseLost)
	}

	// And the store really was left to the new owner: the step is still the
	// SCHEDULED one that owner claimed, not something the zombie overwrote.
	if got := h.job("job_stolen").Step("s").State; got != runmesh.Scheduled {
		t.Errorf("the stolen step is %s, want SCHEDULED: the discarded attempt "+
			"wrote to the store after all", got)
	}

	close(g.release)
	_ = h.eng.Shutdown(t.Context())
}

// finishFailsStore makes Finish fail with an error that is NEITHER ErrLeaseLost
// nor ErrConflict.
//
// The distinction is the whole reason settle has two arms there: the sentinels
// mean "somebody else owns this step and has written its outcome", so the
// result is merely redundant, while anything else — a full disk, a reset
// connection, a constraint nobody anticipated — means the outcome is GONE and
// the lease will have to expire before the step is tried again. Different log
// lines, the same obligation to report the attempt.
type finishFailsStore struct {
	engine.Store
	err error
}

func (s finishFailsStore) Finish(context.Context, runmesh.Outcome) error { return s.err }

func settledWhenTheStoreWriteFails(t *testing.T) {
	obs := newRecordingObserver()
	boom := errors.New("the outcome write hit a full disk")
	h := newHarness(t, tools.Registry{"echo": tools.Echo{}}, withObserver(obs),
		withStore(func(s engine.Store) engine.Store {
			return finishFailsStore{Store: s, err: boom}
		}))

	h.submit("job_lost_write", []runmesh.PlanStep{echoStep("a")}, runmesh.FailFast)
	if n := h.drainAll(); n != 1 {
		t.Fatalf("RunOnce ran %d attempts, want 1: a step whose outcome was never "+
			"written keeps its lease and must not be reclaimable in the same drain", n)
	}

	outcomes := obs.outcomes()
	if len(outcomes) != 1 {
		t.Fatalf("%d attempts settled, want 1: an outcome the store refused is "+
			"still an attempt that ran, and the counter is the only place it shows", len(outcomes))
	}
	o := outcomes[0]
	// SUCCEEDED, not a failure. The step did what it was asked; the failure is
	// the store's, and reporting it as the step's would make a disk incident
	// look like a wave of failing tools.
	if o.State != runmesh.Succeeded {
		t.Errorf("state = %s, want SUCCEEDED: the attempt succeeded and its WRITE "+
			"failed", o.State)
	}
	if o.Discarded || o.Released {
		t.Errorf("a lost outcome reports discarded=%v released=%v; it is neither — "+
			"the write was attempted and it failed", o.Discarded, o.Released)
	}
	if o.Code != "" {
		t.Errorf("code = %q, want empty: the classifier saw a successful attempt", o.Code)
	}

	// Nothing was persisted, which is what makes lease expiry the recovery path
	// rather than anything this counter does.
	if got := h.job("job_lost_write").Step("a").State; got != runmesh.Running {
		t.Errorf("the step is %s, want RUNNING: the injected fault was supposed to "+
			"lose the outcome", got)
	}
}
