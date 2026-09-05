package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/tools"
)

func localExecutor(reg tools.Registry, maxOutput int) tools.Local {
	return tools.Local{Registry: reg, MaxOutputBytes: maxOutput}
}

func input(tool string, params string) tools.Input {
	in := tools.Input{
		JobID: "job_1", StepID: "s", Attempt: 1,
		AttemptID:      runmesh.AttemptID("job_1", "s", 1),
		IdempotencyKey: runmesh.IdempotencyKey("job_1", "s"),
		Tool:           tool,
		Clock:          clock.System(),
	}
	if params != "" {
		in.Params = json.RawMessage(params)
	}
	return in
}

// toolError extracts the classified error a tool or the executor produced.
func toolError(t *testing.T, err error) *runmesh.ToolError {
	t.Helper()
	var te *runmesh.ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error is %T (%v), want *runmesh.ToolError", err, err)
	}
	return te
}

// ------------------------------------------------------------------ registry

func TestRegistryLookupAndAllowlist(t *testing.T) {
	t.Parallel()
	reg := tools.Registry{"echo": tools.Echo{}, "sleep": tools.Sleep{}}

	if !reg.Has("echo") || reg.Has("shell_execute") {
		t.Error("Has is the allowlist predicate handed to plan validation; it disagreed with the registry")
	}
	if _, ok := reg.Lookup("echo"); !ok {
		t.Error("Lookup could not find a registered tool")
	}
	if _, ok := reg.Lookup("nope"); ok {
		t.Error("Lookup found an unregistered tool")
	}
	if got := reg.Names(); len(got) != 2 || got[0] != "echo" || got[1] != "sleep" {
		t.Errorf("Names() = %v, want a sorted list", got)
	}
}

// TestRegistryConcurrentReads pins why Registry is a plain map with no mutex:
// it is built once during wiring and only ever read afterwards.
func TestRegistryConcurrentReads(t *testing.T) {
	t.Parallel()
	reg := tools.Builtins(true)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = reg.Has("echo")
				_, _ = reg.Lookup("sleep")
				_ = reg.Descriptors()
			}
		}()
	}
	wg.Wait()
}

func TestDescriptorsAreCompleteAndSorted(t *testing.T) {
	t.Parallel()
	// undescribed implements Tool but not Describer.
	reg := tools.Registry{"echo": tools.Echo{}, "aaa": undescribed{}}

	got := reg.Descriptors()
	if len(got) != 2 {
		t.Fatalf("%d descriptors, want 2", len(got))
	}
	if got[0].Name != "aaa" || got[1].Name != "echo" {
		t.Errorf("descriptors are not sorted by name: %v", []string{got[0].Name, got[1].Name})
	}
	// A tool without a Describer still appears, with a usable schema: an
	// undocumented tool should be visible and obviously undocumented, not
	// invisible.
	if got[0].Execution != tools.ModeInProcess {
		t.Errorf("undescribed tool execution = %q, want a default", got[0].Execution)
	}
	if len(got[0].InputSchema) == 0 || !json.Valid(got[0].InputSchema) {
		t.Errorf("undescribed tool has no valid input schema: %s", got[0].InputSchema)
	}
	for _, d := range got {
		if !json.Valid(d.InputSchema) {
			t.Errorf("tool %s has an invalid input schema: %s", d.Name, d.InputSchema)
		}
	}
}

type undescribed struct{}

func (undescribed) Run(context.Context, tools.Input) (tools.Output, error) {
	return tools.Output{}, nil
}

// ------------------------------------------------------------------ executor

func TestLocalRejectsUnknownTool(t *testing.T) {
	t.Parallel()
	_, err := localExecutor(tools.Registry{}, 1024).Execute(t.Context(), input("ghost", ""))
	te := toolError(t, err)
	if te.Code != runmesh.CodeToolUnknown {
		t.Errorf("code = %q, want %q", te.Code, runmesh.CodeToolUnknown)
	}
	if te.Retryable {
		t.Error("an unregistered tool is a terminal condition; retrying it would fail identically")
	}
}

// TestLocalContainsPanics: the barrier lives on the tool's own goroutine, so a
// panicking tool becomes an ordinary terminal outcome that still gets written.
func TestLocalContainsPanics(t *testing.T) {
	t.Parallel()
	reg := tools.Registry{"boom": panicking{}}

	var out tools.Output
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped the executor: %v", r)
			}
		}()
		out, err = localExecutor(reg, 1024).Execute(t.Context(), input("boom", ""))
	}()

	if len(out.Result) != 0 {
		t.Errorf("a panicking tool produced a result: %s", out.Result)
	}
	te := toolError(t, err)
	if te.Code != runmesh.CodePanic {
		t.Errorf("code = %q, want %q", te.Code, runmesh.CodePanic)
	}
	if te.Retryable {
		t.Error("a panic was marked retryable; it will panic again")
	}
}

type panicking struct{}

func (panicking) Run(context.Context, tools.Input) (tools.Output, error) {
	panic("tool exploded")
}

func TestLocalCapsOutput(t *testing.T) {
	t.Parallel()
	reg := tools.Registry{"big": sizedTool{n: 200}}

	_, err := localExecutor(reg, 100).Execute(t.Context(), input("big", ""))
	te := toolError(t, err)
	if te.Code != runmesh.CodeOutputTooLarge {
		t.Fatalf("code = %q, want %q", te.Code, runmesh.CodeOutputTooLarge)
	}

	// Under the cap it passes through untouched.
	if _, err := localExecutor(reg, 10_000).Execute(t.Context(), input("big", "")); err != nil {
		t.Fatalf("a result under the cap was rejected: %v", err)
	}

	// A per-step limit overrides the executor default.
	in := input("big", "")
	in.Limits.MaxOutputBytes = 50
	if _, err := localExecutor(reg, 10_000).Execute(t.Context(), in); err == nil {
		t.Error("the per-step output limit was ignored")
	}
}

type sizedTool struct{ n int }

func (s sizedTool) Run(context.Context, tools.Input) (tools.Output, error) {
	return tools.Output{Result: json.RawMessage(`"` + strings.Repeat("x", s.n) + `"`)}, nil
}

// TestLocalRejectsNonJSONResult: the store and the dashboard both assume a
// result is JSON, so bytes that are not are a contract violation rather than
// something to persist and discover later.
func TestLocalRejectsNonJSONResult(t *testing.T) {
	t.Parallel()
	reg := tools.Registry{"bad": rawTool{out: []byte("not json at all")}}

	_, err := localExecutor(reg, 1024).Execute(t.Context(), input("bad", ""))
	te := toolError(t, err)
	if te.Code != runmesh.CodeContractBroken {
		t.Errorf("code = %q, want %q", te.Code, runmesh.CodeContractBroken)
	}

	// An empty result is legitimate: not every tool produces output.
	empty := tools.Registry{"empty": rawTool{}}
	if _, err := localExecutor(empty, 1024).Execute(t.Context(), input("empty", "")); err != nil {
		t.Errorf("a tool returning no result was rejected: %v", err)
	}
}

type rawTool struct{ out []byte }

func (r rawTool) Run(context.Context, tools.Input) (tools.Output, error) {
	return tools.Output{Result: r.out}, nil
}

// TestLocalPassesToolErrorsThrough: the executor must not reclassify what a
// tool already classified, or a retryable failure would become terminal.
func TestLocalPassesToolErrorsThrough(t *testing.T) {
	t.Parallel()
	reg := tools.Registry{"flaky": erroring{err: runmesh.Retry("upstream_503", "gateway")}}

	_, err := localExecutor(reg, 1024).Execute(t.Context(), input("flaky", ""))
	te := toolError(t, err)
	if te.Code != "upstream_503" || !te.Retryable {
		t.Errorf("the executor altered the tool's classification: %+v", te)
	}
}

type erroring struct{ err error }

func (e erroring) Run(context.Context, tools.Input) (tools.Output, error) {
	return tools.Output{}, e.err
}

// TestLocalReturnsWhenContextEnds: a tool blocked on something the test owns
// must not stop Execute from returning once its context is cancelled.
func TestLocalReturnsOnCancellation(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	reg := tools.Registry{"blocked": blocking{ch: block}}
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		_, err := localExecutor(reg, 1024).Execute(ctx, input("blocked", ""))
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Execute returned %v, want context.Canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("Execute did not return after its context was cancelled")
	}
}

type blocking struct{ ch chan struct{} }

func (b blocking) Run(ctx context.Context, in tools.Input) (tools.Output, error) {
	select {
	case <-b.ch:
		return tools.Output{}, nil
	case <-ctx.Done():
		return tools.Output{}, ctx.Err()
	}
}

// ------------------------------------------------------------------ builtins

func TestEchoRoundTrips(t *testing.T) {
	t.Parallel()
	in := input("echo", `{"message":"hello"}`)
	in.Deps = map[string]json.RawMessage{"upstream": json.RawMessage(`{"rows":3}`)}

	out, err := tools.Echo{}.Run(t.Context(), in)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}

	var got struct {
		Echo    json.RawMessage            `json:"echo"`
		JobID   string                     `json:"job_id"`
		StepID  string                     `json:"step_id"`
		Attempt int                        `json:"attempt"`
		Deps    map[string]json.RawMessage `json:"deps"`
	}
	if err := json.Unmarshal(out.Result, &got); err != nil {
		t.Fatalf("decode echo result: %v", err)
	}
	if string(got.Echo) != `{"message":"hello"}` {
		t.Errorf("echo = %s, want the params verbatim", got.Echo)
	}
	if got.JobID != "job_1" || got.StepID != "s" || got.Attempt != 1 {
		t.Errorf("echo lost its identifiers: %+v", got)
	}
	if len(got.Deps) != 1 {
		t.Errorf("echo did not surface its dependency outputs: %v", got.Deps)
	}
}

func TestSleepHonoursTheInjectedClock(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	in := input("sleep", `{"duration":"5s"}`)
	in.Clock = fake

	done := make(chan error, 1)
	go func() {
		_, err := tools.Sleep{}.Run(t.Context(), in)
		done <- err
	}()

	if err := fake.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatalf("sleep did not park on the injected clock: %v", err)
	}
	fake.Advance(5 * time.Second)
	if err := <-done; err != nil {
		t.Fatalf("sleep returned %v", err)
	}
}

func TestSleepReturnsOnCancellation(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC))
	in := input("sleep", `{"seconds":3600}`)
	in.Clock = fake

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := tools.Sleep{}.Run(ctx, in)
		done <- err
	}()

	if err := fake.BlockUntilContext(t.Context(), 1); err != nil {
		t.Fatalf("sleep did not park: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("sleep returned %v, want context.Canceled", err)
	}
}

// TestSleepValidatesAtSubmitTime: a malformed duration must be a 400 now, not
// a step that fails three times in an hour.
func TestSleepValidatesAtSubmitTime(t *testing.T) {
	t.Parallel()
	for _, params := range []string{`{"duration":"forever"}`, `{"seconds":-1}`, `{"duration":"-5s"}`} {
		if err := (tools.Sleep{}).Validate(json.RawMessage(params)); err == nil {
			t.Errorf("Validate(%s) accepted a malformed duration", params)
		}
	}
	for _, params := range []string{`{"duration":"1500ms"}`, `{"seconds":2}`, ``, `{}`} {
		if err := (tools.Sleep{}).Validate(json.RawMessage(params)); err != nil {
			t.Errorf("Validate(%q) rejected a valid duration: %v", params, err)
		}
	}
}

func TestFailInjectsTheRequestedFailure(t *testing.T) {
	t.Parallel()

	// fail_times=2 fails attempts 1 and 2, then succeeds on 3.
	for attempt, wantErr := range map[int]bool{1: true, 2: true, 3: false} {
		in := input("fail", `{"fail_times":2}`)
		in.Attempt = attempt
		_, err := tools.Fail{}.Run(t.Context(), in)
		if (err != nil) != wantErr {
			t.Errorf("attempt %d returned err=%v, want error=%v", attempt, err, wantErr)
		}
		if err != nil && !toolError(t, err).Retryable {
			t.Errorf("attempt %d produced a terminal error; the default mode is retryable", attempt)
		}
	}

	// mode=fatal is terminal.
	in := input("fail", `{"fail_times":9,"mode":"fatal"}`)
	_, err := tools.Fail{}.Run(t.Context(), in)
	if toolError(t, err).Retryable {
		t.Error("mode=fatal produced a retryable error")
	}

	if err := (tools.Fail{}).Validate(json.RawMessage(`{"mode":"explode"}`)); err == nil {
		t.Error("an unknown fail mode was accepted")
	}
}

// TestTestToolsAreGated: a tool whose entire purpose is to panic on demand
// must not exist unless somebody explicitly asked for it.
func TestTestToolsAreGated(t *testing.T) {
	t.Parallel()
	if tools.Builtins(false).Has("fail") {
		t.Error("the fail tool is registered by default")
	}
	if !tools.Builtins(true).Has("fail") {
		t.Error("the fail tool is missing even when test tools are enabled")
	}
	for _, name := range []string{"echo", "sleep"} {
		if !tools.Builtins(false).Has(name) {
			t.Errorf("the %s tool is not registered by default", name)
		}
	}
}

// TestBuiltinsDeclareLimits: the Week-3 Kubernetes Job spec is a translation of
// these values, so they have to exist before the executor that reads them.
func TestBuiltinsDeclareLimits(t *testing.T) {
	t.Parallel()
	for _, d := range tools.Builtins(true).Descriptors() {
		if d.Version == "" {
			t.Errorf("tool %s has no version", d.Name)
		}
		if d.Description == "" {
			t.Errorf("tool %s has no description; it is served to callers and to the planner", d.Name)
		}
		if d.Limits.MaxAttempts < 1 {
			t.Errorf("tool %s declares max_attempts %d", d.Name, d.Limits.MaxAttempts)
		}
		if d.Limits.MaxOutputBytes < 1 {
			t.Errorf("tool %s declares no output cap", d.Name)
		}
		if d.Execution != tools.ModeInProcess {
			t.Errorf("tool %s declares execution %q, want in_process in Week 1", d.Name, d.Execution)
		}
	}
}
