package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kalanas210/runmesh/internal/runmesh"
)

// Builtins returns the tools that ship with the runtime.
//
// The failure-injection tools are gated: a production deployment must not
// expose a tool whose entire purpose is to panic on demand. They exist because
// the interesting behaviour of this system is what it does when things go
// wrong, and that has to be exercisable end to end over the real API — plan
// section 31's failure tests are not unit tests.
func Builtins(enableTestTools bool) Registry {
	r := Registry{
		"echo":  Echo{},
		"sleep": Sleep{},
	}
	if enableTestTools {
		r["fail"] = Fail{}
	}
	return r
}

// Echo returns its parameters. It is the smallest possible real tool: it
// proves the whole path from HTTP submission through claim, lease, execution
// and result persistence without depending on anything external.
type Echo struct{}

func (Echo) Describe() Descriptor {
	return Descriptor{
		Name:        "echo",
		Version:     "1.0",
		Description: "Returns its input unchanged, along with the ids of the step that ran it.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "message": {"type": "string", "description": "Any value; echoed back verbatim."}
  },
  "additionalProperties": true
}`),
		Limits:    Limits{MaxAttempts: 3, MaxOutputBytes: 64 << 10, CPU: "100m", Memory: "64Mi"},
		Execution: ModeInProcess,
	}
}

func (Echo) Run(ctx context.Context, in Input) (Output, error) {
	if err := ctx.Err(); err != nil {
		return Output{}, err
	}
	params := in.Params
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	// Dependency outputs are echoed too, so a multi-step plan built only from
	// echo steps still demonstrates that data flows along the DAG's edges.
	body := struct {
		Echo    json.RawMessage            `json:"echo"`
		JobID   string                     `json:"job_id"`
		StepID  string                     `json:"step_id"`
		Attempt int                        `json:"attempt"`
		Deps    map[string]json.RawMessage `json:"deps,omitempty"`
	}{params, in.JobID, in.StepID, in.Attempt, in.Deps}

	out, err := json.Marshal(body)
	if err != nil {
		return Output{}, runmesh.Fatal(runmesh.CodeContractBroken, "marshal echo result: %v", err)
	}
	return Output{Result: out}, nil
}

// Sleep waits for a configurable duration, honouring cancellation. It exists
// so that concurrency, per-step timeouts and cancellation can be demonstrated
// and load-tested without any external dependency.
type Sleep struct{}

type sleepParams struct {
	Seconds  float64 `json:"seconds,omitempty"`
	Duration string  `json:"duration,omitempty"`
}

func (Sleep) Describe() Descriptor {
	return Descriptor{
		Name:        "sleep",
		Version:     "1.0",
		Description: "Waits for the requested duration, returning early if the step is cancelled.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "seconds":  {"type": "number", "minimum": 0},
    "duration": {"type": "string", "description": "Go duration string, e.g. \"1500ms\". Takes precedence over seconds."}
  },
  "additionalProperties": false
}`),
		Limits:    Limits{MaxAttempts: 3, MaxOutputBytes: 1 << 10, CPU: "50m", Memory: "32Mi"},
		Execution: ModeInProcess,
	}
}

// Validate rejects a malformed duration at submit time, so the caller gets a
// 400 instead of a step that fails three times half an hour later.
func (Sleep) Validate(params json.RawMessage) error {
	_, err := parseSleep(params)
	return err
}

func (Sleep) Run(ctx context.Context, in Input) (Output, error) {
	d, err := parseSleep(in.Params)
	if err != nil {
		return Output{}, runmesh.Fatal(runmesh.CodeContractBroken, "%v", err)
	}
	if err := in.Clock.Sleep(ctx, d); err != nil {
		return Output{}, err
	}
	out, _ := json.Marshal(map[string]any{"slept_ms": d.Milliseconds()})
	return Output{Result: out}, nil
}

func parseSleep(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var p sleepParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, fmt.Errorf("sleep params: %w", err)
	}
	if p.Duration != "" {
		d, err := time.ParseDuration(p.Duration)
		if err != nil {
			return 0, fmt.Errorf("sleep params: duration %q: %w", p.Duration, err)
		}
		if d < 0 {
			return 0, fmt.Errorf("sleep params: duration must not be negative")
		}
		return d, nil
	}
	if p.Seconds < 0 {
		return 0, fmt.Errorf("sleep params: seconds must not be negative")
	}
	return time.Duration(p.Seconds * float64(time.Second)), nil
}

// Fail injects failures on demand. It is registered only when test tools are
// enabled, and it is how the retry ladder, the terminal-error path, the panic
// barrier and the abandoned-tool path are all exercised over the real API.
type Fail struct{}

type failParams struct {
	// FailTimes is how many attempts fail before one succeeds. Attempt numbers
	// are 1-based, so fail_times=2 fails attempts 1 and 2 and succeeds on 3.
	FailTimes int `json:"fail_times,omitempty"`
	// Mode: retryable (default), fatal, panic, or hang.
	Mode string `json:"mode,omitempty"`
	Code string `json:"code,omitempty"`
}

func (Fail) Describe() Descriptor {
	return Descriptor{
		Name:    "fail",
		Version: "1.0",
		Description: "Failure injection for testing retries, terminal errors, panics and " +
			"tools that ignore cancellation. Registered only when RUNMESH_ENABLE_TEST_TOOLS=true.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "fail_times": {"type": "integer", "minimum": 0, "description": "Attempts to fail before succeeding."},
    "mode":       {"type": "string", "enum": ["retryable", "fatal", "panic", "hang"]},
    "code":       {"type": "string", "description": "Error code to report."}
  },
  "additionalProperties": false
}`),
		Limits:    Limits{MaxAttempts: 3, MaxOutputBytes: 1 << 10},
		Execution: ModeInProcess,
	}
}

func (Fail) Validate(params json.RawMessage) error {
	p, err := parseFail(params)
	if err != nil {
		return err
	}
	switch p.Mode {
	case "", "retryable", "fatal", "panic", "hang":
		return nil
	default:
		return fmt.Errorf("fail params: unknown mode %q", p.Mode)
	}
}

func (Fail) Run(ctx context.Context, in Input) (Output, error) {
	p, err := parseFail(in.Params)
	if err != nil {
		return Output{}, runmesh.Fatal(runmesh.CodeContractBroken, "%v", err)
	}
	if in.Attempt > p.FailTimes {
		out, _ := json.Marshal(map[string]any{"succeeded_on_attempt": in.Attempt})
		return Output{Result: out}, nil
	}

	code := p.Code
	if code == "" {
		code = "injected_failure"
	}
	switch p.Mode {
	case "fatal":
		return Output{}, runmesh.Fatal(code, "injected terminal failure on attempt %d", in.Attempt)
	case "panic":
		panic(fmt.Sprintf("injected panic on attempt %d", in.Attempt))
	case "hang":
		// Deliberately ignores ctx. This is the only way to exercise the
		// abandon path, and it is why that path exists: a real tool that
		// blocks on an uninterruptible syscall looks exactly like this.
		<-make(chan struct{})
		return Output{}, nil
	default:
		return Output{}, runmesh.Retry(code, "injected retryable failure on attempt %d", in.Attempt)
	}
}

func parseFail(raw json.RawMessage) (failParams, error) {
	var p failParams
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("fail params: %w", err)
	}
	if p.FailTimes < 0 {
		return p, fmt.Errorf("fail params: fail_times must not be negative")
	}
	return p, nil
}
