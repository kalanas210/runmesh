<div align="center">

# RunMesh

**A Go runtime that turns AI-agent plans into safe, observable, fault-tolerant work.**

[![CI](https://github.com/kalanas210/runmesh/actions/workflows/ci.yml/badge.svg)](https://github.com/kalanas210/runmesh/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)
![Kubernetes](https://img.shields.io/badge/Kubernetes-Jobs-326CE5?logo=kubernetes&logoColor=white)
![Gemini](https://img.shields.io/badge/Planner-Gemini-8E75B2?logo=googlegemini&logoColor=white)
![Next.js](https://img.shields.io/badge/Dashboard-Next.js_16-000000?logo=nextdotjs&logoColor=white)

[What it is](#what-is-runmesh) · [Features](#features) · [How it works](#how-it-works) · [Tech stack](#tech-stack) · [Quick start](#quick-start) · [API](#api) · [Roadmap](#roadmap)

</div>

<br>

![The RunMesh console showing the execution waterfall of an eight-step job: two fetches run side by side, normalise succeeds on its third attempt after two retry backoffs, and three regional totals then run in parallel.](web/public/screenshot-waterfall.png)

---

## What is RunMesh?

AI agents are good at deciding **what** should happen and bad at making it
happen safely. A plan written by a language model still has to run somewhere:
in parallel where it can, in order where it must, isolated from everything it
should not touch, retried when infrastructure fails, and stopped when a person
says stop.

**RunMesh is that execution layer.** Give it a goal in plain English, or a plan
as JSON, and it:

1. turns the goal into a **validated dependency graph** of steps, using Gemini with constrained output;
2. runs every step as an **isolated Kubernetes Job**: non-root, resource-limited and network-denied by default;
3. **survives failure**: timeouts, crashed workers, evicted pods and restarts of RunMesh itself;
4. records a **complete timeline** you can watch live in a dashboard, and exports metrics to Prometheus.

```
goal ──▶ planner ──▶ validated plan ──▶ RunMesh ──▶ isolated execution ──▶ results
                                          │
                        scheduler · policy · leases · retries · events
```

> **The contract: at-least-once execution.** Any step may run more than once — that is what
> makes retries, lease expiry and crash recovery possible without distributed transactions.
> Tools are idempotent, or guarded by an idempotency key. See
> [ADR 0001](docs/decisions/0001-execution-is-at-least-once.md).

### The problems it solves

| Problem | How RunMesh answers it |
|---|---|
| Model-written code runs with real permissions | Every attempt runs in its own pod: non-root, read-only filesystem, no capabilities, no service-account token, default-deny network |
| Agents fan out into many tasks with dependencies | A DAG scheduler runs independent steps concurrently, and a step becomes ready as soon as everything it depends on has succeeded |
| Workers crash, pods get evicted, APIs time out | Leases, heartbeats, retries with backoff and a reconciler recover the work; with PostgreSQL, a killed process loses nothing |
| A model invents tools or asks for more than it should | The allowed tools become the output schema, a generated plan passes the same checks as a hand-written one, and the operator's policy, not the plan, decides resources and network |
| Nobody can say why a run took as long as it did | Every transition is an event: a timeline API, a live stream, an execution waterfall and Prometheus metrics |

---

## Features

### Planning
- **Goal → plan with Gemini.** Constrained decoding against a response schema whose tool field is an enum of the tools the policy allows, so an unknown tool cannot even be decoded.
- **The same gate as a hand-written plan.** A generated plan is admitted by exactly the code that admits a `curl` request: plan validation, tool parameters, execution policy, queue depth and idempotency.
- **A repair loop.** A rejected plan goes back to the model with every problem listed, for up to two rounds by default.
- **Dry run first.** `POST /api/v1/plans` returns the plan with its trace (model, reasoning, token counts and each attempt's problems) and executes nothing.
- **Works without a model.** `RUNMESH_PLANNER=heuristic` turns the URLs in a goal into parallel fetches, an analysis and a report, with no API key.
- **Honest failures.** A goal the model refuses is `422`, a rate limit is `429` with `Retry-After`, and a retired model or rejected key is `503`, never a bare `500`.

### Execution
- **Dependency graphs.** Steps declare `depends_on`; readiness is a query predicate, not a stored state.
- **Bounded concurrency.** A worker pool, and a dispatcher that only claims as many steps as it has idle workers.
- **Timeouts and retries.** Per-step timeouts, exponential backoff with jitter (1 s doubling to at most 60 s by default), and a per-step attempt budget.
- **Cancellation across replicas.** Waiting steps are cancelled at once; running steps learn on their next heartbeat, and their Kubernetes Jobs are deleted.
- **Idempotent submission** with `Idempotency-Key`, and job priorities that order the queue.
- **Failure policies.** `fail_fast` by default, or `continue_on_failure`, which cancels only the failed step's dependents.

### Reliability
- **PostgreSQL is the queue.** `SELECT … FOR UPDATE SKIP LOCKED` hands each ready step to exactly one worker, with no broker to operate.
- **Leases with fencing tokens.** Every write names the lease it holds, so a worker that has lost its lease can never record a result.
- **Crash recovery is tested, not claimed.** A test kills a real server process mid-step, and another server finishes the job.
- **Workload loss is retried.** A pod that is deleted, evicted or preempted gets a fresh attempt (`workload_lost`), while a task that genuinely failed stays failed.
- **Graceful drain.** On shutdown, running steps get time to finish; anything still running goes back to the queue without spending retry budget.
- **Admission control.** Past `RUNMESH_MAX_QUEUE_DEPTH`, submissions get `429` instead of joining a queue that will never drain.

### Isolation and security
- **One Kubernetes Job per attempt**, with `backoffLimit: 0`, `ttlSecondsAfterFinished` cleanup and a backstop deadline.
- **Hardened pods.** UID 65532, a read-only root filesystem (only `/tmp` is writable), all capabilities dropped, no privilege escalation, `seccomp: RuntimeDefault`, no service-account token, and CPU, memory and ephemeral-storage limits.
- **Default-deny NetworkPolicy.** Only pods granted the network get DNS and egress, and never to private, loopback, link-local (cloud metadata) or CGNAT ranges. A script proves the policy is **enforced**, not just applied.
- **SSRF guard** in the task binary: every connection, including each redirect hop, is refused unless the resolved address is public.
- **Execution policy.** A tool *asks* for resources and network, and the operator *grants* them; tool allow and deny lists and image prefixes on top.
- **Scoped API keys** (`jobs.read`, `jobs.write`, `jobs.cancel`, `metrics.read`, `admin`), stored only as SHA-256 digests.

### Observability
- **Execution timeline.** Every state change is an event, with a gap-free sequence number per job.
- **Live stream** over Server-Sent Events: resumable with `Last-Event-ID`, and with PostgreSQL any replica can serve any job's stream.
- **Prometheus metrics** from a hand-written client with closed label vocabularies, so the number of series is bounded and a test holds it under budget.
- **Grafana dashboard** provisioned as code, and structured JSON logs.

### Dashboard
- **Next.js operator console**: overview, job list, live job detail, submit, tools catalogue and runtime health.
- **Execution waterfall** reconstructed from events, so every attempt, retry and backoff is visible, not just the latest.
- **Plan review.** Write a goal, then read and edit the model's plan beside its trace before submitting it; or write the plan JSON yourself.
- **The key stays on the server.** The browser only talks to the console's own origin, and its proxy adds the API key. The console has no login of its own, so run it on localhost or behind your own authentication.

---

## How it works

### Architecture

```mermaid
flowchart TB
    subgraph clients["Clients"]
        UI["Next.js console"]
        AG["AI agent · curl"]
    end

    API["RunMesh API · Go net/http<br/>REST + SSE · planner · scoped keys"]
    GEM(["Gemini API"])
    DB[("PostgreSQL<br/>jobs · steps · events<br/>SKIP LOCKED queue")]
    PROM["Prometheus → Grafana"]

    subgraph engine["Execution engine"]
        DISP["Dispatcher"] --> POOL["Worker pool"]
        REC["Reconciler"]
    end

    subgraph k8s["Kubernetes · one Job per attempt"]
        direction LR
        HTTP["http_request"]
        PY["python_execute"]
        REP["report_generate"]
    end

    UI -->|"same-origin proxy"| API
    AG --> API
    API <-->|"goal → plan"| GEM
    API <-->|"jobs in · timeline out"| DB
    API -.->|"metrics"| PROM
    DB <-->|"claim · heartbeat · settle"| engine
    POOL -->|"create Job · read result"| k8s
```

- **The API** authenticates, validates and persists. It never executes anything itself.
- **The store owns all state.** Every transition is a guarded compare-and-set, and the transition rules are shared code, so the in-memory and PostgreSQL stores pass the same conformance suite.
- **The engine** is three kinds of goroutine: one dispatcher that claims ready steps, a pool of workers that run them, and one reconciler that reclaims leases nobody is renewing.
- **The executor is a seam.** Steps run in-process for development, or as Kubernetes Jobs, and nothing else in the engine changes.

### From a goal to a result

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant API as RunMesh API
    participant Gemini
    participant DB as PostgreSQL
    participant Eng as Engine
    participant K8s as Kubernetes

    User->>API: POST /api/v1/goals
    API->>Gemini: goal, with the allowed tools as a response schema
    Gemini-->>API: plan as structured JSON
    opt the plan fails validation (up to two repair rounds)
        API->>Gemini: the plan and every problem found
        Gemini-->>API: repaired plan
    end
    API->>DB: submit through the same gate as POST /api/v1/jobs
    API-->>User: 201 Created with the job, plan and trace
    loop until every step settles
        Eng->>DB: claim ready steps (SKIP LOCKED) and take leases
        Eng->>K8s: create a hardened Job for each step
        K8s-->>Eng: pod exits, result read from its log
        Eng->>DB: settle the step and append events
    end
    User->>API: GET /api/v1/jobs/{id}/stream
    API-->>User: live events until the job ends
```

### The life of a step

```mermaid
stateDiagram-v2
    [*] --> QUEUED
    QUEUED --> SCHEDULED: claimed, lease granted
    QUEUED --> CANCELLED: cancel requested, or another step failed
    SCHEDULED --> RUNNING: tool started
    RUNNING --> SUCCEEDED: result returned
    RUNNING --> RETRYING: retryable failure or timeout, attempts left
    RUNNING --> FAILED: terminal failure, or attempts used up
    RUNNING --> TIMED_OUT: timed out on the last attempt
    RUNNING --> QUEUED: lease expired, or released on drain
    RUNNING --> CANCELLED: cancel seen on a heartbeat
    RETRYING --> SCHEDULED: backoff elapsed, claimed again
    RETRYING --> CANCELLED: cancel requested, or another step failed
    SUCCEEDED --> [*]
    FAILED --> [*]
    TIMED_OUT --> [*]
    CANCELLED --> [*]
```

A `SCHEDULED` step can also leave by any of `RUNNING`'s exits, because it can be
cancelled, released or lose its lease before its tool starts. There is
deliberately no `BLOCKED` state: a step whose dependencies have not succeeded is
simply not claimable, so nothing has to remember to unblock it.

### When things go wrong

| Situation | What RunMesh does |
|---|---|
| A tool returns a retryable error | `RETRYING`, with exponential backoff and jitter, until its attempts are spent |
| A tool fails for good, or its container exits non-zero | `FAILED`; under `fail_fast` the rest of the job is cancelled, under `continue_on_failure` only its dependents |
| A step runs past its timeout | Retried while attempts remain, then `TIMED_OUT` |
| Its pod is deleted, evicted or preempted, or its Job disappears | Retryable `workload_lost`: a fresh attempt in a fresh pod |
| The RunMesh process crashes | Its leases expire, the reconciler requeues the steps as failed attempts, and another replica or the restarted process finishes them (with PostgreSQL) |
| RunMesh is shut down | Running steps get `RUNMESH_DRAIN_TIMEOUT` to finish, then return to the queue without spending retry budget |
| Somebody cancels the job | Waiting steps are cancelled at once; running steps learn on their next heartbeat, and their workloads are deleted |
| The queue is full | `429 resource_exhausted`: nothing is admitted past `RUNMESH_MAX_QUEUE_DEPTH` |
| The planning model is unavailable | `503 unavailable`, with the provider's reason in `details`; `429` with `Retry-After` when rate limited |
| A plan is invalid | `400 invalid_argument`, with the problems in `details`, before anything runs |

Every row above is covered by an automated test.

---

## Tech stack

| Layer | Technology |
|---|---|
| **Runtime** | Go 1.26: standard-library `net/http` and `log/slog`; the only direct dependencies are `pgx` and the Kubernetes client libraries |
| **State and queue** | PostgreSQL 17: `FOR UPDATE SKIP LOCKED`, embedded migrations, an advisory lock for rolling deploys |
| **Execution** | Kubernetes Jobs via `client-go`; kind + Calico locally; a distroless task image and a Python 3.13 sandbox image |
| **Planning** | Google Gemini API (`gemini-3.6-flash` by default) with structured output, called over `net/http` without an SDK |
| **API** | REST + Server-Sent Events, scoped API keys, idempotency keys |
| **Observability** | Hand-written Prometheus exposition, Prometheus 3.7, Grafana 12.3, JSON logs |
| **Dashboard** | Next.js 16, React 19, TypeScript 5, Tailwind CSS 4, TanStack Query 5, framer-motion 12 |
| **Testing** | Go test with the race detector, a store conformance suite, failure scenarios, kind integration tests, Vitest, k6 |
| **Delivery** | Multi-stage Docker builds onto distroless images, docker compose, GitHub Actions |

**Dependencies are a feature.** There is no Prometheus client library, no
WebSocket library and no Gemini SDK: each would have added a large surface for
a small, well-specified protocol. The Kubernetes libraries stay inside
`internal/k8s` (plus one resource-quantity type in `internal/policy`), and `pgx`
is imported by exactly one production file. The reasoning is in
[ADR 0007](docs/decisions/0007-one-dependency-the-postgres-driver.md) and
[ADR 0012](docs/decisions/0012-metrics-without-a-client-library.md).

---

## Quick start

**Requirements:** Go 1.26+. Docker for PostgreSQL. Node 20.9+ for the dashboard
(CI uses 22). [kind](https://kind.sigs.k8s.io/) and kubectl for Kubernetes
execution. On Windows, every `make <target>` below is `./task.ps1 <target>`.

### 1 · Run it in a minute (in memory)

```bash
export KEY=$(openssl rand -hex 32)
export RUNMESH_API_KEYS="dev=$KEY"    # no scope list: this key can do everything
go run ./cmd/server
```

Submit a diamond-shaped plan: one step, two parallel branches, one join.

```bash
curl -sS -X POST localhost:8080/api/v1/jobs \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d '{
  "name": "diamond",
  "steps": [
    {"id": "fetch",     "tool": "echo",  "params": {"file": "sales.csv"}},
    {"id": "analyze_a", "tool": "sleep", "params": {"duration": "120ms"}, "depends_on": ["fetch"]},
    {"id": "analyze_b", "tool": "sleep", "params": {"duration": "90ms"},  "depends_on": ["fetch"]},
    {"id": "report",    "tool": "echo",  "depends_on": ["analyze_a", "analyze_b"]}
  ]}'
```

The two branches run at the same time, so the job takes one branch's time, not
the sum of both. Without a database the server runs on an in-memory store and
says so at boot, and `GET /api/v1/ready` reports `"durable": false`.

### 2 · Make it durable (PostgreSQL)

```bash
make db-up        # docker compose up -d --wait postgres
export RUNMESH_DATABASE_URL="postgres://runmesh:runmesh@127.0.0.1:5432/runmesh?sslmode=disable"
go run ./cmd/server
```

Migrations are applied at boot. If port 5432 is taken by a local PostgreSQL,
set `RUNMESH_DB_PORT=5434` for compose and use that port in the URL.

### 3 · The full stack: Kubernetes and Gemini

Create a local cluster with an enforcing CNI, then build and load the task images:

```bash
kind create cluster --config deploy/kind/cluster.yaml
kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
kubectl wait --for=condition=Ready node --all --timeout=300s
kubectl apply -f deploy/kubernetes/00-namespaces.yaml -f deploy/kubernetes/10-rbac.yaml -f deploy/kubernetes/20-networkpolicy.yaml

docker build -f deploy/docker/task.Dockerfile   -t runmesh/task:dev .
docker build -f deploy/docker/python.Dockerfile -t runmesh/python:dev .
kind load docker-image runmesh/task:dev runmesh/python:dev --name runmesh

./deploy/kind/verify-networkpolicy.sh     # proves the sandbox network policy is enforced
```

Run RunMesh against it, with a planner:

```bash
export RUNMESH_EXECUTOR=kubernetes RUNMESH_K8S_CONTEXT=kind-runmesh
export RUNMESH_K8S_IMAGE=runmesh/task:dev RUNMESH_PYTHON_IMAGE=runmesh/python:dev
export RUNMESH_POLICY_ALLOW_NETWORK=true
export RUNMESH_PLANNER=gemini RUNMESH_GEMINI_API_KEY=<your key>   # or RUNMESH_PLANNER=heuristic, no key needed
go run ./cmd/server
```

Then hand it a goal:

```bash
curl -sS -X POST localhost:8080/api/v1/goals \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"goal": "Analyze https://raw.githubusercontent.com/jbrownlee/Datasets/master/airline-passengers.csv: the number of months, the total, the busiest month and the trend. Then write a short report."}'
```

Gemini plans a fetch, a Python analysis and a report. Each runs in its own pod,
and `kubectl get pods -n runmesh-tasks -w` shows them come and go.

### 4 · The dashboard

```bash
cd web
cp .env.example .env.local    # set RUNMESH_API_URL and RUNMESH_API_KEY
npm ci && npm run build
npm start -- -H 127.0.0.1
```

Open http://127.0.0.1:3000. The console's proxy holds the API key and has no
login of its own, which is why it binds to localhost here. More in
[`web/README.md`](web/README.md).

### 5 · Metrics

```bash
make monitoring-up    # Prometheus on :9090, Grafana on :3001
```

Grafana's RunMesh dashboard is provisioned at
http://localhost:3001/d/runmesh-overview, with anonymous viewing on. Prometheus
scrapes `/api/v1/metrics` on port 8080 with a bearer token read from a file.
The default file matches the `dev` key in [`.env.example`](.env.example); point
`RUNMESH_SCRAPE_TOKEN_FILE` at your own key, ideally one that holds only
`metrics.read`.

---

## API

Authenticate with `Authorization: Bearer <key>`.

| Method | Path | Purpose | Scope |
|---|---|---|---|
| `POST` | `/api/v1/jobs` | Submit a plan (`Idempotency-Key` supported) | `jobs.write` |
| `GET` | `/api/v1/jobs` | List jobs, newest first, cursor-paged | `jobs.read` |
| `GET` | `/api/v1/jobs/{id}` | A job with its steps | `jobs.read` |
| `POST` | `/api/v1/jobs/{id}/cancel` | Request cancellation (`202`) | `jobs.cancel` |
| `GET` | `/api/v1/jobs/{id}/events` | The execution timeline | `jobs.read` |
| `GET` | `/api/v1/jobs/{id}/stream` | The timeline live, as Server-Sent Events | `jobs.read` |
| `GET` | `/api/v1/tools` | Registered tools and their contracts | `jobs.read` |
| `POST` | `/api/v1/plans` | Goal → validated plan, **executes nothing** | `jobs.write` |
| `POST` | `/api/v1/goals` | Goal → plan → submitted job | `jobs.write` |
| `GET` | `/api/v1/metrics` | Prometheus exposition | `metrics.read` |
| `GET` | `/api/v1/health` | Liveness | — |
| `GET` | `/api/v1/ready` | Readiness, including whether state is durable | — |

Keys are configured as `id[:scope+scope]=secret`, for example
`RUNMESH_API_KEYS=dash:jobs.read=$K1,planner:jobs.read+jobs.write=$K2,ops:admin=$K3`.
A key with no scope list can do everything, and the server names every such key
in a warning at boot.

Errors always use one envelope, with a stable code and a request id:

```json
{
  "error": {
    "code": "invalid_argument",
    "message": "the submitted plan is not valid",
    "details": [{ "field": "steps", "issue": "cycle:a,b" }],
    "request_id": "req_06g71ahayjk1hve4z0v4xeq498"
  }
}
```

Watch a job live:

```bash
curl -N -H "Authorization: Bearer $KEY" localhost:8080/api/v1/jobs/$JOB/stream
```

```
id: 5
event: snapshot
data: {"job":{…},"events":[…],"next_after":5,"truncated":false,"oldest_seq":1}

id: 6
event: event
data: {"seq":6,"job_id":"job_…","step_id":"analyze_a","attempt":1,"type":"STEP_SCHEDULED","state":"SCHEDULED",…}

event: end
data: {"state":"SUCCEEDED"}
```

Quiet streams carry a `: ping` comment every 15 s; `end` means stop reconnecting,
while `bye` (the server is draining) means reconnect from the last id.

A plan step is `{ "id", "tool", "params", "depends_on", "timeout_seconds", "max_attempts" }`,
and a plan adds `name` (required), `priority` and `on_step_failure`. A step that
sets neither gets a 30-second timeout and 3 attempts. Every setting is an
environment variable, validated at boot and documented in
[`.env.example`](.env.example).

---

## Testing

| Suite | What it proves | Size |
|---|---|---|
| Unit and component tests | Domain rules, scheduling, policy, planner, API, metrics, and the Kubernetes executor against a fake cluster | 312 tests, 656 with subtests |
| Store conformance | The in-memory and PostgreSQL stores pass the same cases, including 32 concurrent claimers that must never share a step | 35 cases × both stores |
| Crash recovery | A real server process is killed mid-step, and another server finishes the job | real PostgreSQL |
| Failure scenarios | An orphaned lease is recovered, lease expiry spends retry budget, a cancel reaches every dependent, a full queue answers `429` | 4 scenarios × both stores |
| Kubernetes integration | A step runs in a real pod, a real failure is classified, cancellation deletes the workload, a deleted pod is retried, and the RBAC Role refuses what it should | 6 tests on kind |
| NetworkPolicy probes | Denied pods reach nothing; allowed pods reach the internet but not the cluster API | 4 live probes |
| Dashboard | Waterfall geometry, the event feed, stream parsing, the proxy's origin guard | 280 tests (Vitest) |
| Load | Smoke, sustained submissions, mixed reads and writes, and a soak; thresholds fail the run | 4 k6 scripts |
| Benchmarks | Runtime throughput, claiming, plan validation, telemetry and scrape costs | 9 benchmarks |

```bash
make test               # everything that needs no database
make test-pg            # the whole suite under -race against PostgreSQL
make test-integration   # the failure scenarios
make web-test           # the dashboard's tests
make bench              # the benchmarks
make load               # a k6 smoke run against a server you started
RUNMESH_TEST_KUBECONTEXT=kind-runmesh go test ./internal/k8s -run Integration
```

CI runs gofmt, go vet, staticcheck, the whole suite under the race detector
against PostgreSQL, the failure scenarios, every benchmark once, and the
dashboard's lint, tests and build. The Kubernetes tests and NetworkPolicy probes
need a kind cluster, so they run locally.

About 85% of statements are covered across the packages with tests. Code under
`internal/` gets time from an injected clock: a test that parses the source
rejects direct calls to `time.Now`, `time.Sleep` and timers outside a few marked
exceptions, so timeouts, leases and backoff are tested without waiting for them.

---

## Performance

Measured on one laptop (Intel i7-12650H), with PostgreSQL in Docker Desktop and
fsync off. Read the numbers as shapes, not production capacity: a database round
trip here costs milliseconds, where a real server takes microseconds. Tables,
commands and raw output are in [`docs/benchmarks`](docs/benchmarks/README.md).

| Measurement | Result |
|---|---|
| Steps through the runtime, in memory, 16 workers | **8,890 steps/s** |
| Leases claimed at 32 concurrent claimers (PostgreSQL) | **409 leases/s**, *up* from 270 at one claimer |
| Job submissions sustained over HTTP (in-memory store) | **400/s**, 0 dropped, p95 2.13 ms |
| Mixed API requests across four endpoints | **705/s**, 0 failures |
| Validating a 100-step plan (the default ceiling) | 58 µs |
| Telemetry per settled attempt or HTTP request | ≤ 171 ns, zero allocations |
| One full metrics scrape | 178 µs for 1,577 series (56 KB) |

Throughput rising as claimers are added is the property the design rests on:
`SKIP LOCKED` hands each step to exactly one worker, with no coordinator.

---

## Project layout

```
cmd/server/        wiring: configuration, stores, engine, API, graceful shutdown
cmd/task/          the container tool contract (the task image entrypoint)
internal/
  runmesh/         domain: jobs, steps, states, plans, events, errors (standard library only)
  jobstate/        the transition rules both stores share
  memstore/        in-memory store, for development and fast tests
  pgstore/         PostgreSQL store: SKIP LOCKED queue, fencing, migrations
  storetest/       the conformance suite every store must pass
  engine/          dispatcher, worker pool, reconciler, failure classification
  tools/           tool registry, descriptors and the executor seam
  k8s/             Kubernetes Job executor (the only client-go user)
  policy/          execution policy: requests, grants, sandbox resolution
  planner/         goal → plan pipeline: prompts, schema, validation, repair
  gemini/          Gemini API client over net/http
  httpapi/         REST API, auth, SSE stream, error envelope
  eventbus/        in-process live-event fan-out
  metrics/         Prometheus client and exposition
  report/          Markdown report rendering
  clock/           injected time, and the test that keeps the rest of internal/ off the wall clock
  config/          environment configuration, validated at boot
  bench/           benchmarks, paired so memory and PostgreSQL compare like for like
migrations/        SQL schema, embedded in the binary
deploy/            Dockerfiles, kind cluster, Kubernetes manifests, Prometheus, Grafana
tests/             failure-scenario suite and k6 load scripts
web/               Next.js operator console
docs/              architecture, design decisions, benchmarks, engineering notes
```

---

## Design decisions

The choices with real alternatives are written down as ADRs in
[`docs/decisions`](docs/decisions):

| ADR | Decision |
|---|---|
| [0001](docs/decisions/0001-execution-is-at-least-once.md) | Execution is at-least-once |
| [0002](docs/decisions/0002-postgresql-is-the-queue.md) | PostgreSQL is the queue; Redis is deferred |
| [0003](docs/decisions/0003-kubernetes-jobs-not-pods.md) | Run tasks as Kubernetes Jobs, not raw pods |
| [0004](docs/decisions/0004-standard-library-http.md) | Standard-library `net/http` |
| [0005](docs/decisions/0005-store-owns-transitions.md) | The store owns every state transition |
| [0006](docs/decisions/0006-readiness-is-derived.md) | Readiness is a predicate, not a state |
| [0007](docs/decisions/0007-one-dependency-the-postgres-driver.md) | One database dependency: the PostgreSQL driver |
| [0008](docs/decisions/0008-the-job-row-is-the-lock.md) | The job row is the lock |
| [0009](docs/decisions/0009-the-sandbox-is-the-pod.md) | The sandbox is the pod, not the interpreter |
| [0010](docs/decisions/0010-the-plan-asks-the-operator-grants.md) | The plan asks; the operator grants |
| [0011](docs/decisions/0011-the-model-chooses-the-runtime-decides.md) | The model chooses; the runtime decides |
| [0012](docs/decisions/0012-metrics-without-a-client-library.md) | Metrics without a client library |
| [0013](docs/decisions/0013-server-sent-events-not-websockets.md) | Server-Sent Events, not WebSockets |

---

## Roadmap

Done: the runtime, durable state, Kubernetes isolation, the Gemini planner,
observability, the dashboard, and recovery from crashes and lost workloads.
Next:

- [ ] **Failure details in step errors.** Record a failed container's exit code, termination reason (such as `OOMKilled`) and the end of its log, instead of a generic `tool_broke_contract`.
- [ ] **Written reports.** Have `report_generate` write a narrative summary of its inputs, not only per-step tables.
- [ ] **Adaptive concurrency.** Size the worker pool from observed latency and errors, instead of a fixed `RUNMESH_WORKERS`.
- [ ] **Rate limiting.** Redis-backed limits per tool and per external host, shared across replicas.
- [ ] **Multi-node cluster.** A kind cluster with worker nodes, pod spreading, and node drain and eviction tests.
- [ ] **Cloud deployment.** Manifests for running RunMesh itself on managed Kubernetes with managed PostgreSQL.

---

## Documentation

- [Architecture](docs/architecture/README.md): diagrams, persistence, observability and the live-update path in depth.
- [Engineering notes](docs/engineering-notes.md): how each week was built, and why the hard decisions went the way they did.
- [Design decisions](docs/decisions): the ADRs above.
- [Benchmarks](docs/benchmarks/README.md): methodology, raw results and caveats.
- [Local Kubernetes](deploy/kind/README.md): the kind cluster, Calico, and verifying the sandbox.
- [Dashboard](web/README.md) and [load tests](tests/load/README.md).

<div align="center">
<br>

Built by [kalanas210](https://github.com/kalanas210) · Go, PostgreSQL, Kubernetes and Gemini

</div>
