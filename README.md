# RunMesh

**A Go runtime that turns AI-agent plans into safe, observable, fault-tolerant
work.**

An LLM decides *what* should happen. RunMesh decides *how* it is executed —
concurrently, with dependencies, timeouts, retries, cancellation and a complete
execution timeline.

```
User goal ──▶ planner ──▶ validated plan ──▶ RunMesh ──▶ isolated execution ──▶ results
                                              │
                            scheduler · policy · leases · retries · events
```

> **RunMesh provides at-least-once execution.** Every step may execute more than
> once. Tools must either be idempotent, or be guarded by an idempotency key
> enforced with a unique constraint on `(job_id, step_id, idempotency_key)`.
>
> That sentence is the contract, not an implementation detail. It is what makes
> retries, lease expiry and crash recovery buildable without a distributed
> transaction — see [ADR 0001](docs/decisions/0001-execution-is-at-least-once.md).

---

## Status: Week 1 of 6

| | |
|---|---|
| **Working now** | HTTP API, step DAG, worker pool, per-step timeouts, cancellation, retries with backoff, leases + reconciler, execution timeline, graceful shutdown |
| **In memory** | **A restart loses every job.** PostgreSQL arrives in Week 2 |
| **Dependencies** | **Zero.** `go.mod` has no `require` block |
| **Tests** | 100% pass under `-race`; coverage 82–91% per package |

`GET /api/v1/ready` reports `"durable": false`, and the server logs a warning
at boot. A status endpoint that overstated durability would be worse than no
status endpoint.

<details>
<summary>Roadmap</summary>

| Week | Delivers |
|---|---|
| 1 ✅ | Go API, job/step DAG, worker pool, retries, leases, cancellation, timeouts |
| 2 | PostgreSQL state + `SKIP LOCKED` queue, real crash recovery, scoped API keys |
| 3 | Docker, `kind` + Calico, Kubernetes Job execution via `client-go`, RBAC |
| 4 | Tool sandbox: resource limits, non-root, NetworkPolicy, execution policy |
| 5 | Gemini planner, structured output → schema validation → policy validation |
| 6 | Next.js dashboard with the execution waterfall, Prometheus, k6, benchmarks |

</details>

---

## Quick start

Requires Go 1.25+. Nothing else.

```bash
export RUNMESH_API_KEYS="dev=$(openssl rand -hex 32)"
go run ./cmd/server
```

On Windows PowerShell:

```powershell
$env:RUNMESH_API_KEYS = "dev=0123456789abcdef0123456789abcdef"; go run ./cmd/server
```

Then submit a diamond-shaped plan — one step, two parallel branches, one join:

```bash
curl -sS -X POST localhost:8080/api/v1/jobs \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{
  "name": "csv-report",
  "steps": [
    {"id":"fetch",     "tool":"echo",  "params":{"file":"sales.csv"}},
    {"id":"analyze_a", "tool":"sleep", "params":{"duration":"120ms"}, "depends_on":["fetch"]},
    {"id":"analyze_b", "tool":"sleep", "params":{"duration":"90ms"},  "depends_on":["fetch"]},
    {"id":"report",    "tool":"echo",  "depends_on":["analyze_a","analyze_b"]}
  ]}'
```

```jsonc
{
  "id": "job_06g71af4wx3vdak8enst952h34",
  "state": "SUCCEEDED",
  "duration_ms": 120,          // not 210: the two branches ran at the same time
  "steps": [
    {"id": "fetch",     "state": "SUCCEEDED", "attempt": 1, "duration_ms": 0},
    {"id": "analyze_a", "state": "SUCCEEDED", "attempt": 1, "duration_ms": 120},
    {"id": "analyze_b", "state": "SUCCEEDED", "attempt": 1, "duration_ms": 90},
    {"id": "report",    "state": "SUCCEEDED", "attempt": 1, "duration_ms": 0}
  ]
}
```

The 120 ms total against 210 ms of work is the whole point: `analyze_a` and
`analyze_b` became claimable the instant `fetch` succeeded, and `report` waited
for both.

### The execution timeline

`GET /api/v1/jobs/{id}/events` — every transition, in order, with a gap-free
cursor. This is what the Week-6 dashboard renders as a waterfall.

```
 1  JOB_CREATED                      QUEUED
 2  STEP_SCHEDULED        fetch      SCHEDULED
 3  JOB_STARTED                      RUNNING
 4  STEP_STARTED          fetch      RUNNING
 5  STEP_FINISHED         fetch      SUCCEEDED
 6  STEP_SCHEDULED        analyze_a  SCHEDULED
 7  STEP_SCHEDULED        analyze_b  SCHEDULED   ← both claimed together
 8  STEP_STARTED          analyze_b  RUNNING
 9  STEP_STARTED          analyze_a  RUNNING
10  STEP_FINISHED         analyze_b  SUCCEEDED
11  STEP_FINISHED         analyze_a  SUCCEEDED
12  STEP_SCHEDULED        report     SCHEDULED   ← unblocked by the pair
13  STEP_STARTED          report     RUNNING
14  STEP_FINISHED         report     SUCCEEDED
15  JOB_FINISHED                     SUCCEEDED
```

### Retries

`RUNMESH_ENABLE_TEST_TOOLS=true` registers a `fail` tool for exercising the
failure paths over the real API.

```bash
curl -sS -X POST localhost:8080/api/v1/jobs -H "Authorization: Bearer $KEY" \
  -d '{"name":"flaky","steps":[{"id":"flaky","tool":"fail","params":{"fail_times":2},"max_attempts":3}]}'
```

```json
{ "job": "SUCCEEDED", "step": "SUCCEEDED", "attempt": 3, "failures": 2 }
```

`attempt` and `failures` are separate counters on purpose. `attempt` is
monotonic and names the execution — it becomes the Kubernetes Job name in
Week 3. `failures` is the retry *budget*, and only a classified failure spends
it. Conflating the two is how a rolling restart silently exhausts every
in-flight job's retries.

### Cancellation

```
POST /api/v1/jobs/{id}/cancel  →  202 Accepted
{ "state": "RUNNING", "cancel_requested_at": "…", "cancel_reason": "user",
  "step": "RUNNING" }
```

**202, and the body says `RUNNING`.** Cancellation is a request. A step a
worker already owns keeps running until its next heartbeat delivers the news,
and the heartbeat is the only mechanism that still works in Week 2, when the
cancelling API call lands on a different replica from the step. Honest state
beats fast state.

Moments later:

```json
{ "job": "CANCELLED", "step": "CANCELLED", "failures": 0, "error": "cancelled" }
```

`failures: 0` — cancelling spends no retry budget. Charging somebody a retry
for complying with their own stop request would be absurd.

### Validation

Every plan passes one gate, and it reports *every* problem at once rather than
one per round trip — which matters far more when the author is an LLM retrying
in a loop than when it is a human who would notice.

```json
{
  "error": {
    "code": "invalid_argument",
    "message": "the submitted plan is not valid",
    "details": [{"field": "steps", "issue": "cycle:a,b"}],
    "request_id": "req_06g71ahayjk1hve4z0v4xeq498"
  }
}
```

---

## API

```
POST   /api/v1/jobs                  submit a plan     201 · 200 replay · 400 · 401 · 413 · 429
GET    /api/v1/jobs                  list, keyset-paged        200 · 400 · 401
GET    /api/v1/jobs/{id}             one job with its steps    200 · 401 · 404
POST   /api/v1/jobs/{id}/cancel      request cancellation      202 · 401 · 404 · 409
GET    /api/v1/jobs/{id}/events      execution timeline        200 · 400 · 401 · 404
GET    /api/v1/tools                 registry + contracts      200 · 401
GET    /api/v1/health                liveness                  200        (no auth)
GET    /api/v1/ready                 readiness                 200 · 503  (no auth)
```

Auth is `Authorization: Bearer <key>`. Keys are configured as
`RUNMESH_API_KEYS=id=key,id=key` and stored as sha256 digests, so the lookup
itself leaks no timing information about how many leading bytes of a guess
matched. Every rejection is byte-identical: a caller probing for valid keys
learns nothing about which half of its guess was wrong.

`Idempotency-Key` on a submission makes a client retry safe — a replay returns
the original job with `200` and `Idempotency-Replayed: true`, never a second
job.

---

## How it works

```
                    HTTP handlers                        (no engine access)
                          │  writes plan
                          ▼
                    ┌───────────┐
                    │   Store   │   single owner of state; every transition a
                    └───────────┘   guarded compare-and-set
                     ▲    │    ▲
      claim + lease  │    │    │  outcome
                     │    ▼    │
   ┌─────────────────┴─────────┴──────────────────┐
   │ dispatcher  ──▶  worker pool  ◀──  reconciler │
   │  (1)             (N, bounded)      (1)        │
   └───────────────────────────────────────────────┘
                          │
                          ▼
                    Executor  ──▶  in-process tool  (Week 3: Kubernetes Job)
```

**Readiness is a predicate, not a state.** A step whose dependencies are
unsatisfied sits in `QUEUED`, and the claim query excludes it. There is no
`BLOCKED` state, no materialised pending-dependency counter that can drift, and
therefore no "who unblocks this step" question to get wrong. The predicate is
written once, and it is deliberately the same expression as the Week-2 SQL:

```sql
  job.cancel_requested_at IS NULL
  AND job.state IN ('QUEUED','RUNNING')
  AND step.state IN ('QUEUED','RETRYING')
  AND step.next_attempt_at <= $now
  AND (step.lease_expires_at IS NULL OR step.lease_expires_at <= $now)
  AND NOT EXISTS (SELECT 1 FROM job_steps d
                   WHERE d.job_id = s.job_id AND d.id = ANY(s.depends_on)
                     AND d.state <> 'SUCCEEDED')
ORDER BY job.priority DESC, job.created_at, step.id
FOR UPDATE SKIP LOCKED
```

**Backoff is a column, not a timer.** A retrying step is simply not claimable
until `next_attempt_at`. There is no sleeping goroutine per retrying step, so a
million steps waiting out a backoff cost nothing.

**The goroutine census is fixed at boot:** one dispatcher, one reconciler,
N workers, plus one transient goroutine per in-flight step. No goroutine per
job, no goroutine per request beyond `net/http`'s own, no supervisor tree.

**Backpressure is token accounting.** The dispatcher never claims a step it
does not already hold an idle-worker token for, which is why handing a lease to
a worker can never block indefinitely and why the store is never asked for more
work than the pool can start.

### The four things that are genuinely hard

**1 · A cancelled step must not look like a retryable failure.** The worker
records *why* it stopped **before** cancelling the step's context, and the
classifier consults that reason before it ever reads the tool's error. A tool
returning `Retry("connection reset")` on its way out the door cannot buy itself
a retry, because in that branch its error is never read.

**2 · The outcome write must not inherit the context that was just cancelled.**
Persisting "this step was cancelled" on the context that was cancelled in order
to stop it is how jobs get stuck in `RUNNING` for ever. Every terminal write
goes through `clock.WithWriteDeadline`, which encodes `context.WithoutCancel`
so no call site can forget it — and a `go/parser` test bans
`context.WithTimeout` outside `internal/clock` so no call site can hand-build
the wrong thing instead.

**3 · A worker that lost its lease must write nothing at all.** Every mutation
is guarded on a fencing token *and* the expected state. A stale token gets
`ErrLeaseLost` and the worker discards its result; the state moving on gets
`ErrConflict`. Both branches already exist, so PostgreSQL's behaviour under
real contention is not a new code path.

**4 · A drain must cost zero retries, but a crash must not be free.** Releasing
a step during shutdown is *our* failure and spends no budget; a lease that
simply expired spends one, because a worker that reliably dies on one step must
eventually exhaust `max_attempts` rather than crash-loop the fleet. The two are
asserted side by side in one test named after the asymmetry.

### Testing

```bash
./task.ps1 check     # Windows: lint + race
make check           # everything CI runs
```

There is **no `time.Sleep` anywhere in the suite** — enforced by the same
`go/parser` walk that guards production code. Time is injected, so a test that
exercises a 30-second step timeout finishes in microseconds. Runtime tests
assert on the **event stream**, which is simultaneously the assertion and the
synchronisation primitive: one spurious extra attempt fails the test, where a
final-status check would pass.

The highest-value test here is [`internal/storetest`](internal/storetest):
an **exported conformance suite** the in-memory store must pass today and the
PostgreSQL store must pass *unmodified* in Week 2. Every design claims its
interface is database-ready; this one makes the claim executable, so a
divergence from real `SKIP LOCKED` semantics fails a test rather than surfacing
in production a month later.

> **Windows note.** The race detector needs cgo and a **64-bit** C compiler.
> A 32-bit MinGW on `PATH` fails with *"64-bit mode not compiled in"*.
> `./task.ps1 race` finds a usable toolchain automatically; otherwise
> `choco install mingw`, or run the suite in WSL2. CI runs on Linux, where this
> does not arise.

---

## Configuration

Every knob is an environment variable, validated at boot, with every problem
reported at once. See [`.env.example`](.env.example) for the full list.

`RUNMESH_API_KEYS` is the **only** variable with no default: starting
unauthenticated must not be something a forgotten variable can cause.

Cross-field invariants are checked too, each because violating it produces a
*subtle* failure rather than an obvious one — a heartbeat interval that leaves
fewer than three beats per lease, a store timeout that outlives the drain it is
part of, an abandon grace longer than the lease it is protecting.

---

## Repository

```
cmd/server/          the only wiring in the codebase, and a whole-binary test
internal/
  runmesh/           domain: states, plans, jobs, leases, events. stdlib only
  clock/             the only source of time, plus the purity test enforcing it
  memstore/          in-memory store, written as a faithful SQL simulation
  storetest/         the exported conformance suite pgstore must also pass
  engine/            dispatcher, worker pool, reconciler, and the pure policy
  tools/             the plugin boundary and the Executor seam for Kubernetes
  httpapi/           net/http only; its own narrower view of the store
  config/            every knob, validated at boot
docs/
  architecture/      diagrams and the map of where the engineering is
  decisions/         ADRs for the choices with real alternatives
```

Eleven packages, one binary, zero third-party dependencies.

### Design records

- [0001 — Execution is at-least-once](docs/decisions/0001-execution-is-at-least-once.md)
- [0002 — PostgreSQL is the queue; Redis is deferred](docs/decisions/0002-postgresql-is-the-queue.md)
- [0003 — Execute tasks as Kubernetes Jobs, not raw Pods](docs/decisions/0003-kubernetes-jobs-not-pods.md)
- [0004 — Standard-library `net/http`, zero third-party dependencies](docs/decisions/0004-standard-library-http.md)
- [0005 — The store owns every state transition](docs/decisions/0005-store-owns-transitions.md)
- [0006 — Readiness is a predicate, not a state](docs/decisions/0006-readiness-is-derived.md)

---

## What this is not

RunMesh is not trying to replace Temporal, Celery, Ray, BullMQ or Kubernetes
itself. It is a focused implementation of the execution layer an AI agent
needs, built to be understood end to end and defended in detail: controlled
tool execution, dependency orchestration, failure recovery, and observability
of *why* something took as long as it did.
