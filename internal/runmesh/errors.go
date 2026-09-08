package runmesh

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors returned by any Store implementation. Callers branch on
// these, so the in-memory store and the PostgreSQL store must produce exactly
// the same ones for the same situations — that is what the shared conformance
// suite in internal/storetest exists to prove, and does.
var (
	// ErrNotFound: the job or step does not exist.
	ErrNotFound = errors.New("runmesh: not found")
	// ErrConflict: the fencing token matched but the state did not. The
	// guarded compare-and-set lost. In pgstore: the UPDATE affected zero rows.
	ErrConflict = errors.New("runmesh: state conflict")
	// ErrLeaseLost: the fencing token did not match. Somebody else owns this
	// step now, so the caller must write nothing at all.
	ErrLeaseLost = errors.New("runmesh: lease lost or expired")
	// ErrDuplicate: an idempotency key was replayed. Wraps nothing useful on
	// its own; CreateJob returns it alongside the pre-existing job.
	ErrDuplicate = errors.New("runmesh: duplicate idempotency key")
	// ErrQueueFull: admission control rejected the submission.
	ErrQueueFull = errors.New("runmesh: queue full")
	// ErrClosed: the store has been closed. Every method returns this rather
	// than panicking, so a worker still settling during shutdown fails
	// cleanly.
	ErrClosed = errors.New("runmesh: store closed")
)

// Stable, low-cardinality error codes produced by the runtime itself. Tools
// choose their own codes; these are the ones RunMesh guarantees, which makes
// them safe to alert on and safe to render in the dashboard.
const (
	CodeTimeout        = "step_timeout"
	CodeCancelled      = "cancelled"
	CodeLeaseLost      = "lease_lost"
	CodeAbandoned      = "tool_abandoned"
	CodePanic          = "tool_panic"
	CodeEnginePanic    = "engine_panic"
	CodeUnclassified   = "unclassified"
	CodeContractBroken = "tool_broke_contract"
	CodeToolUnknown    = "tool_unknown"
	CodeOutputTooLarge = "output_too_large"
	CodeDepFailed      = "dependency_failed"
	CodeShutdown       = "shutdown_drain"

	// The execution policy's own codes. They are terminal by construction: no
	// number of retries turns a refusal by policy into permission, and a step
	// that keeps retrying one hides the misconfiguration that caused it.
	//
	// CodeToolDenied: the tool is not on the operator's allowlist, or is on
	// the denylist.
	CodeToolDenied = "tool_denied"
	// CodeToolNotSandboxed: the tool must run in a container and this
	// deployment executes tools in-process.
	CodeToolNotSandboxed = "tool_not_sandboxed"
	// CodeNetworkDenied: the tool asks for egress and this deployment does not
	// grant it to any tool.
	CodeNetworkDenied = "network_denied"
	// CodePolicyViolation: the resolved sandbox is not one the operator's
	// configuration permits — an unparseable quantity, or an image outside the
	// allowed prefixes.
	CodePolicyViolation = "policy_violation"
)

// ToolError is how a tool declares retryability. Anything else a tool returns
// is TERMINAL by default — see engine.Classify. Under an explicitly
// at-least-once contract, silently retrying an error nobody classified is how
// a side-effecting tool ends up running three times.
type ToolError struct {
	Code      string
	Message   string
	Retryable bool
	// RetryAfter is a server hint (an HTTP Retry-After, say). It overrides the
	// computed backoff but is still capped by Backoff.Max, so a hostile
	// upstream cannot park a step for a week.
	RetryAfter time.Duration
	Err        error
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

func (e *ToolError) Unwrap() error { return e.Err }

// Retry marks a failure as worth another attempt after the standard backoff.
func Retry(code, format string, a ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, a...), Retryable: true}
}

// Fatal marks a failure as terminal: no attempt will be made again.
func Fatal(code, format string, a ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// RetryIn is Retry with an explicit delay hint from the failing dependency.
func RetryIn(d time.Duration, code, format string, a ...any) *ToolError {
	return &ToolError{
		Code: code, Message: fmt.Sprintf(format, a...),
		Retryable: true, RetryAfter: d,
	}
}

// Wrap attaches an underlying error so errors.Is/As still reaches it.
func (e *ToolError) Wrap(err error) *ToolError { e.Err = err; return e }

// ErrorInfo is the persisted, JSON-serialisable projection of a failure. It is
// deliberately not an error: it is a record that has to round-trip through a
// jsonb column and through the dashboard in Week 6.
type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Attempt   int    `json:"attempt"`
}

func (e *ErrorInfo) Clone() *ErrorInfo {
	if e == nil {
		return nil
	}
	c := *e
	return &c
}

// ValidationError carries per-field detail straight into the 400 envelope, so
// one request tells the caller everything that is wrong with their plan
// instead of one problem per round trip.
type ValidationError struct{ Details []Detail }

// Detail locates a single problem inside a submitted plan.
type Detail struct {
	Field string `json:"field"` // e.g. "steps[2].depends_on[0]"
	Issue string `json:"issue"` // e.g. "unknown_step"
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("runmesh: plan invalid (%d problems)", len(e.Details))
}
