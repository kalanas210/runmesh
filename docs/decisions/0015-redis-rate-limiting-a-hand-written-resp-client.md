# ADR 0015 — Rate limiting, on a hand-written RESP client

**Status:** Accepted · Roadmap item 5 (2026-09-13) · fills the role
[ADR 0002](0002-postgresql-is-the-queue.md) named for Redis in Phase 3;
applies the dependency test set by
[ADR 0007](0007-one-dependency-the-postgres-driver.md) for the third time,
after [ADR 0012](0012-metrics-without-a-client-library.md) and
[ADR 0014](0014-worker-pool-size-is-adaptive.md)

## Context

ADR 0002 deferred Redis in Week 2, but did not defer it blindly — it named
exactly what would bring it back: "token-bucket rate limiting for external
APIs." The README's Roadmap turned that into a concrete requirement: limits
per tool and per external host, shared across replicas. A single-process
limiter cannot do the second half of that sentence — two RunMesh replicas
each enforcing their own in-memory bucket for the same host would each allow
the configured rate, and the host would see twice it — so this is the one
roadmap item that genuinely needs a second network dependency, not a design
choice this codebase could route around the way it routed around one for
metrics.

Three things decided about the existing engine before any Redis-specific
question is reached.

**The engine does not know what a tool's parameters mean.** `tools.Input`
carries `Params json.RawMessage`, opaque past the tool boundary. "Per external
host" is meaningless without reading a URL out of a specific tool's specific
field, and the engine learning how to do that for `http_request` would be
exactly the coupling `internal/tools`' registry exists to prevent — a second
tool with a second idea of what its own network parameter is called would
need the engine changed to support it.

**A refusal has to land before `Executor.Execute`, not around it.** The
Kubernetes executor's `Execute` is what creates the Job; a rate-limit check
that ran after would still pay for a pod the operator's own limit just said
should not exist. `Sandbox.Limits` already occupies exactly that point in
`invoke` — checked, and capable of stopping the tool from ever running — for
the same reason a `policy.Engine` refusal does.

**Observability may never apply backpressure to execution**, the same rule
`engine.Observer`'s own doc states and ADR 0014's controller was built around.
Whatever asks Redis "is this attempt allowed" runs on a goroutine holding a
worker-pool capacity token, and a slow answer is not merely a slow rate
limiter — it shrinks the pool the same way a slow Observer would.

## Decision

**A hand-written RESP2 client, `internal/redis`, for the same reason
`internal/metrics` hand-writes Prometheus exposition instead of taking
`client_golang`.** RESP is a five-type wire grammar — status, error, integer,
bulk string, array, each with one null variant — not the roughly 1,500 lines
of handshake and SCRAM-SHA-256 that justified taking `pgx` in ADR 0007. The
whole protocol is `resp.go`: encode a command as an array of bulk strings,
decode one of five reply shapes. `client.go` adds a connection, AUTH/SELECT at
dial time, and a small pool — borrow, use, return; close and free the slot on
error — sized so concurrent rate-limit checks from concurrent workers do not
serialise through one connection, which would make this package the thing
that shrinks the pool it exists to protect.

**No timeout of its own.** `internal/clock`'s purity test bans `time.Now` and
`context.WithTimeout` outside package `clock`, and `internal/redis` obeys that
like everything else under `internal/`: every deadline a call observes comes
from the caller's `ctx`, via `net.Dialer.DialContext` for the handshake and
`ctx.Deadline()` mirrored onto `net.Conn.SetDeadline` for everything after.
The caller — `engine.checkRateLimit` — derives that `ctx` with
`clock.WithWriteDeadline(ctx, cfg.RateLimitTimeout)`, exactly the shape every
store call in the engine already uses.

**Token bucket, run atomically inside Redis via `EVAL`, always inline —
never `EVALSHA`/`SCRIPT LOAD`.** Two replicas racing a plain
GET/compute/SET over one bucket key would both read the same balance and
both believe they spent the last token; `EVAL` makes the whole read-refill-
spend-write sequence one atomic step, which is the entire reason to reach for
a script instead of a handful of commands. Sending the ~40-line script text
inline rather than caching its SHA1 costs a few hundred bytes per rate-limit
check — a control-plane call that happens once per attempt, not once per
request a fleet serves — against implementing `SCRIPT LOAD`, a client-side
SHA1 the client must independently be able to derive to re-load after a
reconnect, and `NOSCRIPT` cache-miss handling: real protocol surface this
role does not need. The script itself asks Redis's own `TIME` for "now"
rather than trusting the caller's clock, because every replica asking the
same server for the time is the one clock they can all agree on; a caller-
supplied timestamp would let ordinary clock skew between RunMesh instances
turn into rate-limit skew.

**Two dimensions, two buckets, checked in order.** `internal/ratelimit`
exposes `Rule{RatePerSecond, Burst}` for each, independently enabled by
whether both are non-zero, so a deployment can meter a host without metering
a tool or the reverse. `Allow` checks `PerTool` first, `PerHost` second, and
stops at the first refusal — an attempt that will not run has no reason to
spend a second bucket's token finding that out. `PerHost` reads the target
host from exactly one tool it is told how to parse, `http_request`'s own
`url` parameter, mirroring the same field `cmd/task/http.go`'s SSRF guard
already reads; a tool this package does not name is simply exempt from
`PerHost`, because guessing at an unknown tool's parameter shape is how a
rate limiter starts limiting the wrong field or panicking on one that was
never there.

**Enforced by the control plane, never by the task pod.** The check happens
inside `invoke`, on the API server's own process, before a Kubernetes Job is
ever created — not inside the sandboxed task binary that actually makes the
HTTP call. A sandboxed pod reaching Redis would mean adding it to every task
pod's egress, regardless of what that tool's own descriptor asked for,
which is exactly the kind of always-on exception the default-deny
NetworkPolicy and the execution-policy's per-tool grant both exist to
refuse. The trust boundary this keeps is the same one `internal/policy`
already draws: the operator's decisions live in the trusted process; the
sandbox gets only what its own attempt was granted.

**A refusal is retryable, and unreachable Redis is a DIFFERENT retryable
code from an actually-empty bucket.** `CodeRateLimited` carries the bucket's
own estimate of when it will have a token again, via `runmesh.RetryIn` — the
same server-hint mechanism a tool's own `Retry-After` already uses.
`CodeRateLimitUnavailable`, for when Redis itself could not be reached
inside `RateLimitTimeout`, carries no hint and falls through to `Classify`'s
ordinary exponential backoff instead: a fixed retry delay is the wrong
answer to an outage that might last longer than one bucket's refill time,
and letting the existing curve grow on repeated failures is exactly the
behaviour an unavailable dependency should get. Unreachable Redis is
FAIL-CLOSED — treated as a refusal, not as unlimited — the same posture
`RUNMESH_POLICY_ALLOW_NETWORK`'s own default takes: a runtime that cannot
confirm a limit should not assume there isn't one.

**Two error codes are the entire new Observer surface — there is no new
Observer method.** A refusal is a `*runmesh.ToolError`, which travels the
identical `settle` → `Classify` → `AttemptSettled` path every other tool
failure does, so `runmesh_step_attempt_failures_total{tool=…,
code="rate_limited"}` already counts it and the job's timeline already
carries the reason. A dedicated `RateLimitChecked` callback, the shape
ADR 0014's `ConcurrencyAdjusted` needed for a decision with no attempt
outcome to ride on, would here be exactly the two-paths-to-one-number drift
`internal/metrics`' own doc warns against.

## Consequences

- **Six new `RUNMESH_*` variables** — a URL, four `Rate`/`Burst` pairs, a
  timeout, a key prefix — at the density `RUNMESH_BACKOFF_*` and item 4's
  `RUNMESH_CONCURRENCY_*` already set. `RUNMESH_REDIS_URL` unset is the whole
  off switch: every other rate-limit variable is refused at boot if set
  without it (`config.Validate`), because a rate or a burst with nowhere to
  keep bucket state is a misconfiguration worth a boot failure naming the
  variable, the same posture `RUNMESH_DATABASE_URL` already takes for an
  unreachable store.
- **`docker-compose.yml` gains a `redis` service behind its own `ratelimit`
  profile**, exactly like `monitoring`: `db-up` and every other compose
  command already in this file keep pulling and waiting for nothing new. No
  named volume — bucket state is disposable by design, so a restart costing
  every bucket its balance is the correct, conservative failure mode, not a
  loss to guard against.
- **`internal/engine` gains a tenth-and-eleventh-method-free seam.**
  `RateLimiter` is one method, declared in `internal/engine/ratelimit.go` the
  same way `Store`, `Sandbox` and `Observer` each are, defaulting to
  `nopRateLimiter` exactly as `policy.Passthrough` and `nopObserver` already
  do — every existing test and every deployment that predates this feature
  is unaffected by its arrival for the same reason.
- **A pointer-vs-interface trap lives in exactly one place.**
  `*ratelimit.Limiter` is nil precisely when `RUNMESH_REDIS_URL` is unset,
  and assigning a nil pointer straight into `engine.Deps.RateLimiter` (an
  interface field) would produce a non-nil interface wrapping that nil
  pointer — `engine.New`'s own `== nil` check would miss it, and the first
  `Allow` call would panic on a nil receiver instead of ever reaching
  `nopRateLimiter`. `cmd/server/run.go` is the one file that has to know
  this, in a four-line comment at the one call site it matters.
- **`internal/redis` and `internal/ratelimit` join the PostgreSQL suite's
  own shape.** `RUNMESH_TEST_REDIS_URL` unset skips both, exactly the way
  `RUNMESH_TEST_DATABASE_URL` already gates `internal/pgstore`;
  `TestRedisIsConfiguredInCI` is `TestDatabaseIsConfiguredInCI`'s own
  sentence with the noun changed, and CI's `redis:` service sits beside its
  `postgres:` one so the skip is never a lie there either.
- **`internal/redis`'s test suite is real-server-only, by choice.** There is
  no mock Redis and no fake clock standing in for it: `resp.go`'s own
  encode/decode is pure and unit-tested with no network at all, but
  `client.go`'s pooling, AUTH/SELECT and `EVAL` round trips are proven
  against a real `redis:7-alpine`, the same trust `internal/pgstore` places
  in a real PostgreSQL rather than a mock `database/sql` driver.

## Reopening condition

**RESP3 and `HELLO`.** This client speaks RESP2 only — the reply grammar
Redis has answered with for every version this feature could reasonably run
against — and gets everything it needs from it. RESP3's richer types (maps,
sets, doubles, push messages) earn their cost only if a future role for
Redis — the pub/sub fan-out or the operational cache ADR 0002 also named —
needs one of them; the rate limiter does not.

**`rediss://` (TLS).** `ParseURL` recognises and refuses it today, naming
this ADR, rather than silently downgrading to plain text. `crypto/tls` is
standard library, so this is client work, not a dependency question — it is
simply not built, because every environment this ships to today terminates
TLS in front of Redis or reaches it over a private network.

**Redis Cluster.** A single-node `EVAL` is enough for the traffic this
feature targets. Cluster mode changes the addressing model — a script's keys
must hash to the same slot — which is a real design question this codebase
has not needed to answer yet.

**Per-tool or per-host override tables.** Today's `Rule` is one rate and one
burst per dimension, applied uniformly to every tool name or every host that
dimension sees. A configuration file mapping specific tool names or host
patterns to their own limits is a natural extension and was scoped out of
this pass the same way `internal/policy`'s own per-tool ceilings started as
global values before growing allow/deny lists.

## Alternatives considered

- **`github.com/redis/go-redis` or another client library, used directly.**
  Fails ADR 0007's containment test before the protocol-complexity question
  is even reached: `redis.Client`, `redis.Cmdable` and the library's own
  error types would appear in `internal/ratelimit`, in whatever type
  `engine.Deps` accepts, and in `cmd/server/run.go`'s wiring — the engine
  naming a third-party type is precisely the objection ADR 0007 raised
  against `pgxpool` and ADR 0012 raised against `client_golang`.
- **A library confined behind `internal/redis`'s own interface.** The real
  alternative, and it still fails the same test `client_golang` did in
  ADR 0012: the confinement solves the cheap part (serialising RESP frames,
  a few hundred lines against a five-type grammar) and not the expensive
  part (which two commands this role actually needs, and the atomicity
  argument for `EVAL`), while still paying for a client that speaks Pub/Sub,
  Cluster, Sentinel and Streams this role uses none of.
- **A sliding-window or fixed-window counter instead of a token bucket.** A
  fixed window is bursty exactly at its boundary — the last request of one
  window and the first of the next can land a millisecond apart and both
  succeed, doubling the effective rate right there. A sliding-window log is
  more precise and needs a sorted set per key plus a trim on every check —
  more Redis round trips and more protocol surface for a precision this
  role does not need. Token bucket was also the specific mechanism ADR 0002
  named, and nothing about building it changed that judgment.
- **Check the rate limit from inside the sandboxed task binary, next to the
  SSRF guard it shares a URL-parsing shape with.** Rejected in the Decision
  above: it would mean every task pod's egress including Redis regardless
  of what its own tool was granted, which undermines the default-deny
  NetworkPolicy and the per-tool network grant `internal/policy` exists to
  enforce. The engine already resolves the security envelope per attempt,
  before the pod exists; the rate limit is one more thing resolved there.
- **Fail OPEN when Redis is unreachable, admitting every attempt rather
  than refusing it.** Available Redis is common relative to a database
  outage in most deployments' failure profiles, which makes fail-open look
  attractive — but a rate limiter that goes unlimited exactly when its own
  backing store is unhealthy is a limiter that fails at the one moment
  failing matters, and `RUNMESH_POLICY_ALLOW_NETWORK`'s own default already
  states this codebase's answer to "assume permission or assume refusal
  when unsure." Fail-closed, with the ordinary backoff curve rather than a
  fixed wait, is the position that stays consistent with it.
- **A new `Observer.RateLimitChecked` method, mirroring
  `ConcurrencyAdjusted`.** `ConcurrencyAdjusted` earned its method because a
  resize decision has no attempt outcome to ride on — nothing else in the
  engine already reports it. A rate-limit refusal is the opposite case: it
  IS an attempt outcome, with a code and a tool and a duration, and already
  flows through `AttemptSettled`. Adding a second counter fed by the same
  event is the drift `internal/metrics`' own doc names, not a gap.
