# RunMesh architecture

> An LLM decides **what** should be done. RunMesh decides **how** it is executed —
> safely, reliably, concurrently and observably.

## System view

```mermaid
flowchart TB
    UI["Next.js dashboard<br/>(Week 6)"] --> API
    Agent["Gemini planner<br/>(Week 5)"] --> API

    API["Go API<br/>net/http + WebSocket"] --> ORCH

    subgraph ORCH["Orchestrator"]
        direction LR
        PLAN["Plan validation"] --> SCHED["Scheduler<br/>DAG readiness"]
        SCHED --> POL["Policy engine<br/>limits + allowlist"]
        POL --> EXEC["Execution manager"]
    end

    ORCH --> STORE[("PostgreSQL<br/>state + SKIP LOCKED queue")]
    ORCH --> OBS["Observability<br/>Prometheus / slog"]
    ORCH --> REDIS[("Redis<br/>rate limit + pub/sub<br/>Phase 3")]

    EXEC --> K8S["Kubernetes"]
    K8S --> P1["Task Job<br/>python sandbox"]
    K8S --> P2["Task Job<br/>http tool"]
    K8S --> P3["Task Job<br/>report"]
```

## Execution lifecycle

```mermaid
sequenceDiagram
    participant C as Client
    participant A as API
    participant S as Store
    participant Q as Queue
    participant W as Worker
    participant T as Tool

    C->>A: POST /api/v1/jobs
    A->>A: validate plan (schema + policy)
    A->>S: persist job + steps (QUEUED)
    A-->>C: 201 Created {job_id}

    loop until no ready steps
        W->>Q: claim ready step (lease)
        Q->>S: QUEUED -> SCHEDULED (guarded, SKIP LOCKED)
        W->>T: execute(ctx with deadline)
        alt success
            T-->>W: result
            W->>S: RUNNING -> SUCCEEDED + unlock dependents
        else retryable failure
            T-->>W: retryable error
            W->>S: RUNNING -> RETRYING (backoff) or FAILED at max attempts
        else timeout / cancel
            W->>S: RUNNING -> TIMED_OUT / CANCELLED
        end
    end

    C->>A: GET /api/v1/jobs/{id}
    A->>S: read job + steps + events
    A-->>C: 200 {status, steps, timeline}
```

## Job state machine

```mermaid
stateDiagram-v2
    [*] --> QUEUED
    QUEUED --> SCHEDULED: claimed by worker
    QUEUED --> CANCELLED: cancel requested
    SCHEDULED --> RUNNING: execution started
    SCHEDULED --> CANCELLED: cancel requested
    RUNNING --> SUCCEEDED: tool returned a result
    RUNNING --> RETRYING: retryable failure, attempts remain
    RUNNING --> FAILED: terminal failure or attempts exhausted
    RUNNING --> TIMED_OUT: step deadline exceeded
    RUNNING --> CANCELLED: cancel requested
    RETRYING --> QUEUED: backoff elapsed
    RETRYING --> CANCELLED: cancel requested
    TIMED_OUT --> RETRYING: retryable by policy
    SUCCEEDED --> [*]
    FAILED --> [*]
    CANCELLED --> [*]
```

## Where the interesting engineering is

| Concern | Mechanism | Introduced |
|---|---|---|
| Concurrency | Bounded worker pool, goroutines + channels + `select` | Week 1 |
| Cancellation | `context.Context` propagated API → scheduler → worker → tool | Week 1 |
| Timeouts | Per-step deadline derived from the tool contract | Week 1 |
| Dependencies | Step DAG; a step becomes ready when all `depends_on` succeed | Week 1 |
| Failure policy | Error taxonomy → retryable vs terminal; exponential backoff | Week 1 |
| Durability | PostgreSQL store; queue via `SELECT … FOR UPDATE SKIP LOCKED` | Week 2 |
| Crash recovery | Leases + heartbeats + a reconciler loop over expired leases | Week 2 |
| Authorisation | Scoped API keys, enforced from a route table a test walks | Week 2 |
| Isolation | Kubernetes Jobs: non-root, resource limits, NetworkPolicy | Week 3–4 |
| Planning | Gemini structured output → schema validation → policy validation | Week 5 |
| Observability | Event timeline, Prometheus metrics, execution waterfall UI | Week 6 |

## Persistence

Three tables, and the lock protocol that keeps them consistent.

```mermaid
erDiagram
    jobs ||--o{ job_steps : "has"
    jobs ||--o{ job_events : "has"

    jobs {
        text id PK "k-sortable, so the PK b-tree appends"
        text state
        int priority
        timestamptz cancel_requested_at "a flag, not a state"
        text idempotency_key UK "partial unique index"
        bigint event_seq "bumped with the transition"
    }
    job_steps {
        text job_id PK, FK
        text id PK "author-supplied, unique in the job"
        text_array depends_on "the readiness predicate reads this"
        text state
        int attempt "monotonic: names the execution"
        int failures "the retry budget"
        timestamptz next_attempt_at "the backoff IS this column"
        text lease_id "the fencing token"
        timestamptz lease_expires_at
    }
    job_events {
        bigserial global_seq PK "store-wide cursor"
        text job_id FK
        bigint seq "per-job, 1-based, gap-free"
        text type
    }
```

**The `jobs` row is the lock.** Every mutating path takes it first and only then
touches step rows, so the two natural lock orders — `Claim` wanting steps then
job, `Finish` wanting job then step — cannot form a cycle. The sweeps take it
through `FOR UPDATE OF j SKIP LOCKED`; `Heartbeat` is the one write that takes
no job lock at all, because it is the hottest path in the system and cannot be
half of a cycle. See [ADR 0008](../decisions/0008-the-job-row-is-the-lock.md).

**Neither store implements the transition policy.** Both call
`internal/jobstate`, so the in-memory and PostgreSQL stores cannot disagree
about what a failure dooms or what a cancellation touches. `internal/storetest`
then covers what genuinely differs between them.

## Design records

Decisions with real alternatives are recorded in [`docs/decisions`](../decisions):

- [0001 — Execution is at-least-once](../decisions/0001-execution-is-at-least-once.md)
- [0002 — PostgreSQL is the queue; Redis is deferred](../decisions/0002-postgresql-is-the-queue.md)
- [0003 — Execute tasks as Kubernetes Jobs, not raw Pods](../decisions/0003-kubernetes-jobs-not-pods.md)
- [0004 — Standard-library `net/http`, zero third-party dependencies](../decisions/0004-standard-library-http.md)
- [0005 — The store owns every state transition](../decisions/0005-store-owns-transitions.md)
- [0006 — Readiness is a predicate, not a state](../decisions/0006-readiness-is-derived.md)
- [0007 — One dependency: the PostgreSQL driver](../decisions/0007-one-dependency-the-postgres-driver.md)
- [0008 — The job row is the lock](../decisions/0008-the-job-row-is-the-lock.md)
