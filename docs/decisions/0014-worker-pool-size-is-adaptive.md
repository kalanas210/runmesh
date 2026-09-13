# ADR 0014 — The worker pool sizes itself between Workers and MaxWorkers

**Status:** Accepted · Roadmap item 4 (2026-09-13) · mechanism sits beside
[ADR 0008](0008-the-job-row-is-the-lock.md)'s token accounting; policy sits
beside [ADR 0011](0011-the-model-chooses-the-runtime-decides.md)'s
model/runtime split

## Context

`RUNMESH_WORKERS` has been a single fixed number since Week 1: the engine
spawns that many goroutines, seeds that many capacity tokens, and the pool
never moves for the life of the process. The README's Roadmap named the
problem this leaves open: "size the worker pool from observed latency and
errors, instead of a fixed RUNMESH_WORKERS." An operator who under-sizes it
leaves throughput on the table during a quiet hour; one who over-sizes it for
a quiet hour has over-provisioned it for the DB connections, CPU and memory
every worker can spend the instant load actually arrives — `internal/bench`'s
own benchmark says as much: "the honest answer to 'should I raise it' is a
curve, not a number."

Three things about the existing engine decide the shape of the fix before any
algorithm is chosen.

**Concurrency is already a token count, not a goroutine count.** `idle` holds
one `struct{}` per free worker; a worker is "in the pool" exactly when it
holds one. Nothing about that model requires the token count to be fixed —
only that it never exceeds however many goroutines exist to spend them.

**The goroutine census is a stated invariant.** The package doc fixes it at
boot, and `internal/metrics/gauges.go` reads it back in the same words to
argue the runtime collector's `go_goroutines` gauge is "a live assertion of a
stated design invariant." A design that spawns or kills worker goroutines at
runtime would contradict that on every resize, silently, for a property
nothing mechanically checks.

**Observability may never apply backpressure to execution.** `engine.Observer`
already states this once, for the reasons `internal/memstore/subscribe.go`
states it for the event bus. Whatever reads AttemptSettled to decide a resize
has to obey the same rule the existing nine methods do.

## Decision

**Workers stays the floor and the starting size; MaxWorkers is a new,
optional ceiling.** `RUNMESH_MAX_WORKERS` defaults to `RUNMESH_WORKERS`, which
makes the policy below mathematically constant at the existing fixed value —
adaptive sizing is opted INTO by widening the ceiling, never a behaviour
change to a deployment that never asked for it.

**The goroutine census stays fixed at MaxWorkers, not Workers.** `Start` spawns
`MaxWorkers` worker goroutines up front — the census invariant is preserved,
just restated at the wider number — but seeds only `Workers` capacity tokens.
The rest of the goroutines are already parked on the shared lease channel,
exactly as able to receive a step as any other; growing the pool means minting
more tokens, never starting more goroutines. Shrinking is the same idea run
backwards: a token due to be retired is not one currently in a worker's hand,
so a shrink is never a revocation of work already in flight. It is expressed
as **debt**, in `resize.go`: `retireDebt` is incremented, and the next
`returnToken` calls that debt is still outstanding are the ones that pay it —
by not returning their token to `idle` at all — rather than by anyone
reaching into a channel that a worker might be mid-send to. Growing cancels
outstanding debt before minting anything new, so a shrink immediately followed
by a grow costs no tokens either way.

**The policy is AIMD** (additive increase, multiplicative decrease) — the same
shape TCP congestion control uses, and for the same reason: a healthy pool
only needs to be *found*, one worker at a time, while a failing one needs to
shed load *now*. `Concurrency.Next` (`concurrency.go`) is pure — no clock, no
shared state — and takes exactly three things: the current size, one window's
summary (attempts, errors, latency sum), and the caller's running-minimum
latency baseline. A window whose error rate crosses `ConcurrencyErrorRate`
halves the pool immediately, rounding up so `Min` stays reachable. Otherwise,
if the window's mean latency has risen more than `ConcurrencyHeadroom` above
the best window ever seen, growth merely *pauses* — a slow window is not
necessarily a failing one, and shrinking the very capacity that could work off
a queue would be exactly backwards. Otherwise the pool grows by `Step`. An
empty window changes nothing: silence is not evidence either way.

**The window is fed from the same value every other consumer of an outcome
already sees.** `settle` builds one `AttemptOutcome`, and this is the fourth
thing to read it, after the Observer, the "step settled" log line and the
persisted `runmesh.Outcome`. `concurrencySignal` (`resize.go`) excludes
Discarded (another worker already owned the step — its duration is how long
*this* worker took to find that out, not a tool's latency) and Released
(mostly a drain, where every in-flight step reports at once and would read as
a correlated mass failure at the exact moment the pool is shutting down
anyway) and Cancelled (an operator asked for that, which says nothing about
capacity).

**The controller is its own goroutine, gated on having a range to work in.**
`runConcurrency` ticks every `ConcurrencyInterval`, snapshots the window,
calls `Concurrency.Next`, and calls `resize` only when the target actually
moved. `Start` does not launch it at all when `MaxWorkers == Workers`, and
that gate is load-bearing rather than an optimisation: `clock.Fake` counts
registered waiters, and several existing tests synchronise on an *exact* count
via `BlockUntilContext` — an always-on ticker registered for the life of every
engine would have added one to that count forever and desynchronised every one
of them. (It did, the first time this shipped: `TestAbandonedToolDoesNotParkAWorker`
and others went from a reliable ~1s run to an intermittent multi-minute
timeout, because `BlockUntilContext(ctx, 4)` was satisfied by the extra ticker
before the timer the test actually cared about had registered. A pristine
worktree at the parent commit, run five times, never took more than 1.1s; the
same suite with an always-on controller hung on 3 of 4 runs. Gating the
goroutine on `MaxWorkers > Workers` fixed it in five more runs at ~1s each,
because a deployment that never sets `RUNMESH_MAX_WORKERS` now starts exactly
the goroutines it started before this feature existed.)

**A resize is observable through the existing seam, not a new one bolted on.**
`Observer` gains a tenth method, `ConcurrencyAdjusted(from, to int)`, called at
most once per interval and only on an actual move — matching the "declared by
its consumer" shape `Store`, `Sandbox` and the existing nine methods already
have. `internal/metrics` counts it as `runmesh_concurrency_adjustments_total`,
cut by direction, on the same reasoning `runmesh_dispatcher_wakeups_total`
already uses for hint-vs-tick: the *level* is already `runmesh_workers` (now
reporting the live size rather than the static config value, so the fixed-pool
case reads exactly as it always has), and a second gauge fed by the same
events would be the two-paths-to-one-number drift this codebase's own metrics
file warns against. What a level cannot show is the *rate* a real move
happens at — that is what the counter is for.

## Consequences

- **`RUNMESH_CLAIM_BATCH` and `RUNMESH_DB_MAX_OPEN_CONNS` are now bounded by
  `MaxWorkers`, not `Workers`.** A claim batch or a connection pool still
  capped at the floor would be a ceiling on the feature the wider one exists to
  enable — a dispatcher that grew its pool to 32 workers but can still only
  claim 8 leases a round trip just takes four round trips to fill the capacity
  it has, and a connection pool sized to the floor can deadlock the pool's own
  drain under the ceiling's true load. Both defaults are unchanged
  (`ClaimBatch` still defaults to `Workers`; `DBMaxOpenConns` to
  `MaxWorkers + 4`), so a deployment that never sets `RUNMESH_MAX_WORKERS` sees
  no numeric change at all — only the *legal range* an operator may configure
  widened.
- **Five new `RUNMESH_*` variables**, at the density `RUNMESH_BACKOFF_*`
  already set: a ceiling, an interval, and the three `Concurrency` knobs.
  `ConcurrencyErrorRate` and `ConcurrencyHeadroom` are deliberately NOT
  defaulted away from zero in `engine.Config.setDefaults` — a zero is itself a
  real, intentional setting (never shrink on errors; any latency above
  baseline pauses growth), the same way `RUNMESH_BACKOFF_JITTER=0` is real —
  so the operator-facing default of 0.2 / 0.5 lives only in `config.go`'s
  loader, exactly where `BACKOFF_JITTER`'s does.
- **A worker returning its token is no longer always a channel send.** Every
  site that used to write `idle <- struct{}{}` directly — `runWorker`,
  `giveBack` — now calls `returnToken`, which is a lock-free CAS loop against
  `retireDebt` before it is ever a send. `giveBack`'s existing "can never
  block" proof still holds: `idle`'s buffer is `MaxWorkers`, and `resize`
  never mints past `size()`, which is itself clamped to `MaxWorkers` by
  `Concurrency.clamp`.
- **`Stats().Workers` and `GET /api/v1/ready` now report the live size, not
  the static config.** For a deployment that never sets
  `RUNMESH_MAX_WORKERS` this is the same number it always reported, because
  the live size never leaves `Workers`. For one that does, an operator reading
  readiness now sees what the pool actually is, not what it started as.
- **`runmesh_workers`'s meaning widens rather than changes.** It already
  existed; it now reports the adaptive size instead of a constant, and a new
  `runmesh_workers_max` gauge reports the ceiling next to it —
  `runmesh_workers == runmesh_workers_max` is the signal that adaptive sizing
  is effectively off, on a dashboard that used to have no way to ask.

## Reopening condition

The AIMD policy reacts to *this process's own* attempts, which is right for a
single replica and blind on purpose to what a fleet is doing: two replicas
under the same database load will size independently and can converge on
different pool sizes for identical traffic. If a future week needs fleet-wide
coordinated sizing — sharing a window across replicas, or bounding total DB
connections across a horizontally-scaled deployment rather than per-process —
that is a distributed problem this ADR does not attempt, and it is exactly the
kind of shared, cross-replica state the next roadmap item (Redis-backed rate
limiting) is already bringing into the codebase for a different reason.

## Alternatives considered

- **A goroutine pool that actually spawns and kills workers on resize.**
  Rejected first: it contradicts the package doc's fixed goroutine census, a
  property `internal/metrics`'s runtime collector reads back as a live
  assertion. It also reintroduces exactly the `wg.Add`/`Wait` race the
  existing comment on `Start` calls out — `Add` must happen before any
  goroutine that might call `Done` exists, which a runtime-spawned worker
  cannot guarantee against a concurrent `Shutdown`.
- **A single global counter with no debt, decremented directly on shrink.**
  Simpler to describe, wrong to build: decrementing the live capacity count
  while a worker is *between* taking a lease and returning its token has no
  token to remove without either blocking the shrink until one happens to be
  free (unbounded latency on a decision that is supposed to be immediate) or
  reaching into a channel a `select` elsewhere might be mid-send to (a data
  race on the channel's internal state, not just on Go values). Debt sidesteps
  both: it is satisfied by whichever token returns next, from whichever
  goroutine that happens to be, with no coordination beyond one CAS.
- **Percentile-based (p95/p99) latency instead of the window mean.** A
  fixed-size histogram per window is real bookkeeping — bucket boundaries, a
  reset each interval — for a controller that only needs to know two things:
  is the window failing, and is it slower than the best window on record. The
  mean answers both without the machinery, and `internal/metrics` already owns
  the percentile story for humans; this policy does not need to duplicate it
  to make a sizing decision.
- **Feed the controller from `internal/metrics` instead of a second
  accumulator.** Rejected on the same grounds ADR 0012 keeps `internal/engine`
  from importing `internal/metrics` at all: the engine is the dependency root,
  metrics is a consumer of it, and a controller that read Prometheus state to
  drive its own runtime would invert that. `concurrencyAccumulator` is
  intentionally as small as `claimErrors`/`leasesExpired` — the engine's own
  bookkeeping, not a second telemetry system.
- **Shrink gradually, one worker every interval, instead of halving.** Matches
  growth's caution but not the asymmetry the whole policy exists to encode: an
  error budget being spent right now is not a trend to confirm over several
  more windows, and TCP's own AIMD does not shrink gradually either for the
  same reason.
- **Let `ClaimBatch` and `DBMaxOpenConns` continue to derive from `Workers`.**
  Would have shipped a ceiling the dispatcher and the connection pool could
  not actually use — the numeric ceiling this ADR exists to make real.
