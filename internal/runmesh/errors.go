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
	CodeTimeout   = "step_timeout"
	CodeCancelled = "cancelled"
	CodeLeaseLost = "lease_lost"
	// CodeWorkloadLost: the sandbox running the attempt was taken away rather
	// than failing on its own — its Job deleted, or its pod deleted, evicted or
	// preempted. Retryable: the step did not finish, and the at-least-once
	// contract already allows it to run again.
	CodeWorkloadLost   = "workload_lost"
	CodeAbandoned      = "tool_abandoned"
	CodePanic          = "tool_panic"
	CodeEnginePanic    = "engine_panic"
	CodeUnclassified   = "unclassified"
	CodeContractBroken = "tool_broke_contract"
	CodeToolUnknown    = "tool_unknown"
	CodeOutputTooLarge = "output_too_large"
	CodeDepFailed      = "dependency_failed"
	CodeShutdown       = "shutdown_drain"

	// A sandboxed task's container stopped on its own, and badly. Both are
	// terminal, and both carry an ExitInfo saying how.
	//
	// CodeTaskExited: the container exited non-zero. The next attempt would run
	// the same code against the same input, and under the at-least-once
	// contract a failure nobody understood is not something to repeat.
	CodeTaskExited = "task_exited"
	// CodeTaskOOMKilled: the kernel killed the container at its memory limit.
	// The limit is the operator's grant, and the next attempt meets exactly the
	// same one.
	CodeTaskOOMKilled = "task_oom_killed"

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

	// The rate limiter's own codes. Both are RETRYABLE, which is what tells
	// them apart from the policy codes just above: a denylist entry stays
	// wrong forever, a rate limit stops being wrong the moment its bucket
	// refills.
	//
	// CodeRateLimited: a configured bucket - per tool or per external host -
	// had no token left. The error message names which.
	CodeRateLimited = "rate_limited"
	// CodeRateLimitUnavailable: the limiter itself (Redis) could not be
	// reached. Distinct from CodeRateLimited so an operator reading
	// runmesh_step_attempt_failures_total can tell "traffic is high, working
	// as intended" from "the rate limiter is down, go fix it" without having
	// to read message text.
	CodeRateLimitUnavailable = "rate_limit_unavailable"
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
	// Exit is how a sandboxed task's container stopped, when that is what
	// failed. engine.Classify carries it into the persisted ErrorInfo.
	Exit *ExitInfo
	Err  error
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
	// Exit is set only when a sandboxed task's container stopped badly: nil for
	// every other failure, and for every in-process tool.
	Exit *ExitInfo `json:"exit,omitempty"`
}

// ExitInfo is what Kubernetes recorded about a task container that stopped:
// the facts an operator would otherwise need `kubectl describe pod` and
// `kubectl logs` for, both of which are gone once the Job's TTL reclaims it.
type ExitInfo struct {
	// Code is the container's exit code. 137 is SIGKILL, which is also what an
	// OOM kill looks like from inside the container.
	Code int32 `json:"code"`
	// Reason is the kubelet's word for it: Error, OOMKilled, ContainerCannotRun.
	Reason string `json:"reason,omitempty"`
	// LogTail is the end of the container's output, as the kubelet kept it.
	// It is whatever the task printed, so a tool must not print secrets: the
	// rule its result line already lives under.
	LogTail string `json:"log_tail,omitempty"`
}

// Clone returns a copy that shares no memory with e, so a delivered event can
// never alias a stored step's error.
func (e *ErrorInfo) Clone() *ErrorInfo {
	if e == nil {
		return nil
	}
	c := *e
	if e.Exit != nil {
		exit := *e.Exit
		c.Exit = &exit
	}
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
