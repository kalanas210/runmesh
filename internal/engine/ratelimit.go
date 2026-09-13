package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/runmesh"
)

// RateLimiter is consulted once per attempt, at exactly the point Sandbox is:
// inside invoke, after the security envelope resolves and before the tool
// actually runs. That placement is the whole point — a refusal here must stop
// a Kubernetes Job from ever being created, not merely stop its result from
// counting, which is why this is checked before Executor.Execute rather than
// wrapped around it.
//
// Declared HERE by its consumer for the same reason Store, Sandbox and
// Observer are: internal/engine does not import internal/ratelimit, and a
// test satisfies this with one method. params is the step's raw JSON — the
// same value tools.Input.Params carries — because "per external host" is
// meaningless without it: a limiter that only ever saw the tool name could
// not tell one http_request step's target from another's, and the engine
// itself must not learn how to parse a tool's parameters to make that
// possible generically. See internal/ratelimit for the one tool this ships
// knowing how to read a host out of.
type RateLimiter interface {
	Allow(ctx context.Context, tool string, params json.RawMessage) (RateDecision, error)
}

// RateDecision is one Allow call's answer.
type RateDecision struct {
	Allowed bool
	// RetryAfter is when the bucket that refused this will have a token
	// again. Zero is a legitimate value the caller must not treat as "no
	// wait" when Allowed is false — see checkRateLimit, which reads it only
	// in that case.
	RetryAfter time.Duration
	// Key names which bucket decided this — "tool:http_request" or
	// "host:api.example.com" — for the classified error's message. There is
	// deliberately no separate Observer method for this: a refusal becomes a
	// *runmesh.ToolError with CodeRateLimited, which travels the ordinary
	// AttemptSettled path every other failure does, so
	// runmesh_step_attempt_failures_total{tool=…, code="rate_limited"} is
	// already the count and Key is already the human-readable reason on the
	// timeline. A second counter fed by the same event would be exactly the
	// drift internal/metrics' own doc warns against.
	Key string
}

// nopRateLimiter is the default: every tool, every host, unlimited. It is
// why RUNMESH_REDIS_URL unset changes nothing about how a step runs, the same
// shape policy.Passthrough gives Sandbox and nopObserver gives Observer.
type nopRateLimiter struct{}

func (nopRateLimiter) Allow(context.Context, string, json.RawMessage) (RateDecision, error) {
	return RateDecision{Allowed: true}, nil
}

var _ RateLimiter = nopRateLimiter{}

// checkRateLimit is invoke's rate-limit gate. A refusal is RETRYABLE, never
// terminal — unlike a Sandbox refusal, which stays wrong on every attempt, a
// bucket that is empty now has a token later, and RetryAfter says exactly
// when. A limiter that could not be reached at all gets no RetryIn: it falls
// through to Classify's ordinary backoff curve instead, which grows on
// repeated failures the way a fixed retry hint on an outage should not.
func (e *Engine) checkRateLimit(ctx context.Context, l runmesh.Lease) error {
	rctx, cancel := clock.WithWriteDeadline(ctx, e.cfg.RateLimitTimeout)
	defer cancel()

	d, err := e.limiter.Allow(rctx, l.Tool, l.Params)
	if err != nil {
		return runmesh.Retry(runmesh.CodeRateLimitUnavailable,
			"rate limiter unreachable: %v", err)
	}
	if !d.Allowed {
		return runmesh.RetryIn(d.RetryAfter, runmesh.CodeRateLimited,
			"rate limit exceeded (%s)", d.Key)
	}
	return nil
}
