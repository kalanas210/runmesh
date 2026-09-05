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
    A-->>C: 202 Accepted {job_id}

    loop until no ready steps
        W->>Q: claim ready step (lease)
        Q->>S: QUEUED -> RUNNING (guarded)
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
| Isolation | Kubernetes Jobs: non-root, resource limits, NetworkPolicy | Week 3–4 |
| Planning | Gemini structured output → schema validation → policy validation | Week 5 |
| Observability | Event timeline, Prometheus metrics, execution waterfall UI | Week 6 |

## Design records

Decisions with real alternatives are recorded in [`docs/decisions`](../decisions):

- [0001 — Execution is at-least-once](../decisions/0001-execution-is-at-least-once.md)
- [0002 — PostgreSQL is the queue; Redis is deferred](../decisions/0002-postgresql-is-the-queue.md)
- [0003 — Execute tasks as Kubernetes Jobs, not raw Pods](../decisions/0003-kubernetes-jobs-not-pods.md)
- [0004 — Standard-library `net/http`, zero third-party dependencies](../decisions/0004-standard-library-http.md)
