// Package tools is the plugin boundary: everything RunMesh knows how to
// execute, and the contract every executable thing must satisfy.
//
// The engine depends on Executor and never on Tool. That indirection is the
// Kubernetes seam: in Week 3 a k8sjob.Executor is registered and the
// dispatcher, worker pool, lease handling, classification and state machine
// are untouched.
package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
)

// Tool is one method, because a tool is one behaviour.
//
// The contract, which the tests in this package enforce:
//
//   - MUST honour ctx and return promptly once it is done. A tool that ignores
//     cancellation is detected and killed off after the abandon grace, and its
//     step is failed with code tool_abandoned.
//   - MUST NOT retry internally. RunMesh owns retries; a tool that also
//     retries produces two nested retry budgets and an execution history the
//     dashboard cannot reconstruct.
//   - SHOULD classify failures with runmesh.Retry, runmesh.Fatal or
//     runmesh.RetryIn. An unclassified error is treated as TERMINAL — see
//     engine.Classify for why that default points this way.
//   - MAY be invoked more than once for the same step. Execution is
//     at-least-once. Use Input.IdempotencyKey, which is stable across attempts.
type Tool interface {
	Run(ctx context.Context, in Input) (Output, error)
}

// Describer is an optional capability, in the standard library's own idiom
// (compare http.Flusher): a tool opts in, and the caller type-asserts. No
// registration ceremony and no reflection.
type Describer interface{ Describe() Descriptor }

// Validator runs at SUBMIT time, so a malformed plan is a 400 rather than a
// step that fails three times at runtime an hour later. It is also where
// per-tool input validation (plan section 33.6) hangs.
type Validator interface {
	Validate(params json.RawMessage) error
}

// ExecutionMode says where a tool runs. Week 1 has one value; the constant for
// the other exists now so the descriptor shape does not change in Week 3.
type ExecutionMode string

const (
	ModeInProcess ExecutionMode = "in_process" // Week 1
	ModeContainer ExecutionMode = "container"  // Week 3: a Kubernetes Job
)

// Limits are carried in Week 1; only Timeout, MaxAttempts and MaxOutputBytes
// are read. CPU, Memory, Image and Network exist now so that Week 3's Job spec
// is a translation of a value that already exists, rather than a new concept
// threaded through five layers under time pressure.
type Limits struct {
	Timeout        time.Duration `json:"-"`
	MaxAttempts    int           `json:"max_attempts"`
	MaxOutputBytes int           `json:"max_output_bytes"`
	CPU            string        `json:"cpu,omitempty"`    // "500m"
	Memory         string        `json:"memory,omitempty"` // "256Mi"
	Image          string        `json:"image,omitempty"`
	Network        bool          `json:"network"`
}

// Descriptor is what GET /api/v1/tools serves and, in Week 5, what is handed
// to Gemini as a function declaration.
type Descriptor struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Limits      Limits          `json:"limits"`
	Execution   ExecutionMode   `json:"execution"`
}

// Input is everything a tool is allowed to know.
//
// Note what is absent: no Store, no *Job, no engine handle. A tool cannot
// mutate runtime state, by construction rather than by convention.
type Input struct {
	JobID   string
	StepID  string
	Attempt int

	// AttemptID names THIS execution (job.step.N) and becomes the Week-3
	// Kubernetes Job name. IdempotencyKey is stable across attempts of this
	// step; it is the value that makes retrying a side-effecting tool safe.
	AttemptID      string
	IdempotencyKey string

	Tool   string
	Params json.RawMessage
	// Deps carries the results of this step's depends_on steps, keyed by step
	// id, so a tool consumes upstream output without being handed a store.
	Deps   map[string]json.RawMessage
	Limits Limits

	Log   *slog.Logger // pre-tagged with job, step and attempt
	Clock clock.Clock  // tools that wait use this, never time.Sleep
}

// Output is what a tool produces. Result must be valid JSON or nil; the
// executor rejects anything else as a contract violation rather than storing
// bytes the dashboard cannot render.
type Output struct {
	Result json.RawMessage
	Meta   map[string]string
}
