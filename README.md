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

## Status: Week 4 of 6

| | |
|---|---|
| **Working now** | HTTP API, step DAG, worker pool, per-step timeouts, cancellation, retries with backoff, leases + reconciler, execution timeline, graceful shutdown |
| **Durable** | PostgreSQL state and a `SKIP LOCKED` queue. A killed process loses nothing: its leases expire and another replica finishes the work |
| **Isolated** | One Kubernetes Job per attempt, `backoffLimit: 0`. Non-root, read-only root filesystem, all capabilities dropped, seccomp, cpu/memory/ephemeral limits, and a default-deny NetworkPolicy — with a script that proves the policy is enforced rather than merely applied |
| **Governed** | An execution policy an LLM-authored plan cannot widen: the tool asks, the operator grants, and the grant is resolved on **every attempt** ([ADR 0010](docs/decisions/0010-the-plan-asks-the-operator-grants.md)) |
| **Auth** | Scoped API keys — `jobs.read`, `jobs.write`, `jobs.cancel`, `admin` |
| **Dependencies** | The PostgreSQL driver and `client-go`. The driver is imported in exactly one file, reached only through `database/sql` ([ADR 0007](docs/decisions/0007-one-dependency-the-postgres-driver.md)); `client-go` is confined to `internal/k8s` |
| **Tests** | 215 tests, 388 cases, all green under `-race`. 92% of the policy engine, 88% of the API, 84% of the engine. Includes the store conformance suite, a crash-recovery test against real PostgreSQL, and a test that reads the shipped NetworkPolicy manifests and fails if they stop matching the labels the code sets |

Without `RUNMESH_DATABASE_URL` the server still runs on the in-memory store for
development — and says so, loudly, at boot and in `GET /api/v1/ready`
(`"durable": false`). A URL that is set but unreachable **fails the boot**: it
never falls back, because silently starting non-durable is how an outage turns
into data loss.

<details>
<summary>Roadmap</summary>

| Week | Delivers |
|---|---|
| 1 ✅ | Go API, job/step DAG, worker pool, retries, leases, cancellation, timeouts |
| 2 ✅ | PostgreSQL state + `SKIP LOCKED` queue, real crash recovery, scoped API keys |
| 3 ✅ | Docker, `kind` + Calico, Kubernetes Job execution via `client-go`, RBAC |
| 4 ✅ | Tool sandbox: resource limits, non-root, NetworkPolicy, execution policy |
| 5 | Gemini planner, structured output → schema validation → policy validation |
| 6 | Next.js dashboard with the execution waterfall, Prometheus, k6, benchmarks |

</details>

---

## Quick start

Requires Go 1.25+ and Docker for the database.

```bash
docker compose up -d --wait postgres

export RUNMESH_API_KEYS="dev=$(openssl rand -hex 32)"
export RUNMESH_DATABASE_URL="postgres://runmesh:runmesh@127.0.0.1:5432/runmesh?sslmode=disable"
go run ./cmd/server
```

Migrations are embedded in the binary and applied at boot, under an advisory
lock so a rolling deploy of five replicas runs them exactly once.

If a PostgreSQL is already installed on the host it owns 5432 (and often 5433
too), so the container cannot bind. Publish it somewhere else — the port inside
the container never moves:

```bash
RUNMESH_DB_PORT=5434 docker compose up -d --wait postgres
make test-pg RUNMESH_DB_PORT=5434        # ./task.ps1 test-pg on Windows
```

Worth knowing, because the symptom is misleading: compose reports the container
*healthy* while connections quietly reach the other PostgreSQL, and the error is
`password authentication failed`. The health check runs inside the container and
cannot see who owns the host port.

Without Docker, drop `RUNMESH_DATABASE_URL` and everything below still works —
against the in-memory store, which loses every job on restart and tells you so.

On Windows PowerShell:

```powershell
$env:RUNMESH_API_KEYS = "dev=0123456789abcdef0123456789abcdef"
$env:RUNMESH_DATABASE_URL = "postgres://runmesh:runmesh@127.0.0.1:5432/runmesh?sslmode=disable"
go run ./cmd/server
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
  "duration_ms": 198,          // not 210+: the two branches ran at the same time
  "steps": [
    {"id": "fetch",     "state": "SUCCEEDED", "attempt": 1, "duration_ms": 8},
    {"id": "analyze_a", "state": "SUCCEEDED", "attempt": 1, "duration_ms": 130},
    {"id": "analyze_b", "state": "SUCCEEDED", "attempt": 1, "duration_ms": 108},
    {"id": "report",    "state": "SUCCEEDED", "attempt": 1, "duration_ms": 5}
  ]
}
```

`analyze_a` and `analyze_b` became claimable the instant `fetch` succeeded, and
`report` waited for both — so the job costs one 120 ms branch rather than the
sum of two.

Those are real numbers from a local PostgreSQL, and they are **honest about what
durability costs**: roughly 70–100 ms of the total is round trips, because each
step is now a claim, a start, heartbeats and a finish, each its own transaction.
The in-memory store finishes the same plan in about 120 ms. Somebody will ask
which number the README should show; it should show this one, because this is
the one where a restart does not lose the job. The first submission after a boot
is slower again (~430 ms) while the pool warms and PostgreSQL caches the plans.

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
and the heartbeat is the only mechanism that still works now that the cancelling
API call can land on a different replica from the step — which, with a shared
database, it can. Honest state beats fast state.

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
                                                       scope                statuses
POST   /api/v1/jobs           submit a plan            jobs.write    201 · 200 replay · 400 · 401 · 403 · 413 · 429
GET    /api/v1/jobs           list, keyset-paged       jobs.read     200 · 400 · 401 · 403
GET    /api/v1/jobs/{id}      one job with its steps   jobs.read     200 · 401 · 403 · 404
POST   /api/v1/jobs/{id}/cancel   request cancellation jobs.cancel   202 · 401 · 403 · 404 · 409
GET    /api/v1/jobs/{id}/events   execution timeline   jobs.read     200 · 400 · 401 · 403 · 404
GET    /api/v1/tools          registry + contracts     jobs.read     200 · 401 · 403
GET    /api/v1/health         liveness                 —             200
GET    /api/v1/ready          readiness                —             200 · 503
```

Auth is `Authorization: Bearer <key>`. Keys are stored as sha256 digests, so the
lookup itself leaks no timing information about how many leading bytes of a
guess matched. Every **401** is byte-identical: a caller probing for valid keys
learns nothing about which half of its guess was wrong.

A **403** is deliberately the opposite. The credential was accepted, so the
answer names the scope that was missing — retrying with the same key will never
work, and an authenticated caller may as well be told what to ask for.

```bash
RUNMESH_API_KEYS=dash:jobs.read=$K1,planner:jobs.read+jobs.write=$K2,ops:admin=$K3
```

`jobs.cancel` is separate from `jobs.write` on purpose: submitting work and
stopping somebody else's are different authorities, and an agent in a retry loop
should not be able to halt the fleet. A key with no scope list is granted
everything — `dev=<key>` from Week 1 still works, because an upgrade that
silently stripped every running key of its access would be a worse failure than
a permissive default — and the server names every such key in a warning at boot.

The route table in [`internal/httpapi/api.go`](internal/httpapi/api.go) is the
whole of the authorisation policy, as data. A test walks it and fails on any
route that requires no scope without being one of the two probes, and a second
test walks every route against every scope, so a new endpoint reachable by a
read-only key is a test failure rather than an audit finding.

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
therefore no "who unblocks this step" question to get wrong. Here it is, as it
actually runs:

```sql
SELECT s.job_id, s.id
  FROM job_steps s
  JOIN jobs j ON j.id = s.job_id
 WHERE     j.cancel_requested_at IS NULL
       AND j.state IN ('QUEUED','RUNNING')
       AND s.state IN ('QUEUED','RETRYING')
       AND s.next_attempt_at <= $1
       AND (s.lease_expires_at IS NULL OR s.lease_expires_at <= $1)
       AND NOT EXISTS (
             SELECT 1 FROM unnest(s.depends_on) AS dep (id)
              WHERE NOT EXISTS (
                    SELECT 1 FROM job_steps d
                     WHERE d.job_id = s.job_id AND d.id = dep.id
                       AND d.state = 'SUCCEEDED'))
 ORDER BY j.priority DESC, j.created_at, j.id, s.id
 LIMIT $2
 FOR UPDATE OF j SKIP LOCKED
```

The dependency clause is doubly negated — *there is no dependency that is not a
`SUCCEEDED` step* — rather than the shorter `AND d.state <> 'SUCCEEDED'`. The
short form treats a dependency naming a step that does not exist as *satisfied*:
no row, nothing found, nothing blocks. Plan validation makes that impossible
today, which is exactly why it is the kind of assumption that stops being true
later. `jobstate.Claimable` is the same expression in Go, and both stores run
against the same conformance suite.

`FOR UPDATE OF **j**` — the job, not the step — is the lock protocol, and it is
the difference between a system that deadlocks under load and one that cannot.
See [ADR 0008](docs/decisions/0008-the-job-row-is-the-lock.md).

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

### What Week 2 actually changed

Three things, and only one of them is "we added a database".

**A second store is two places for the policy to be wrong.** The subtlest code
in this project is not the SQL — it is the transition policy: the fail-fast
trigger that has to fire from *every* writer, the cancel propagation that must
not touch a leased step, the doomed-dependent sweep without which
`continue_on_failure` hangs for ever. Reimplementing that in SQL and trusting a
test suite to catch divergence catches only the cases somebody thought to write
down. So it was extracted instead: [`internal/jobstate`](internal/jobstate) is
pure, stdlib-only, and both stores call it. **They do not agree by testing; they
agree by being the same code.** What [`internal/storetest`](internal/storetest)
then proves is the part that genuinely differs — the predicate as real
`SKIP LOCKED` SQL, fencing under real contention, paging over real rows.

**One lock, taken first, everywhere.** `Claim` naturally wants a step row and
then the job row; `Finish` naturally wants the job row and then a step row. That
is a textbook ABBA deadlock, and it would surface only under production
concurrency. Every mutating path now takes the `jobs` row first — the sweeps get
there through `FOR UPDATE OF j SKIP LOCKED` — so the cycle cannot be built. The
one exception is `Heartbeat`, the hottest write in the system, which extends a
lease on a step nothing else contends for and so cannot be half of a cycle.
[ADR 0008](docs/decisions/0008-the-job-row-is-the-lock.md) has the reasoning and
what it costs.

**Crash recovery stopped being a claim.** Week 1 had leases, a reconciler and a
documented recovery path, none of which could ever run: the store died with the
process, so the sweep always woke to an empty world.
[`cmd/server/crash_test.go`](cmd/server/crash_test.go) now starts a real server
as a subprocess, waits until a step is genuinely `RUNNING`, and **kills it** — no
signal handler, no drain, the lease left in the database with nobody to renew
it. A different process, against the same database, finishes the job. The test
then asserts the crash spent exactly **one** unit of retry budget, because a
worker that reliably dies on one step has to exhaust `max_attempts` rather than
crash-loop the fleet — while a graceful drain, asserted beside it, spends none.

### What Week 4 actually changed

Week 3 put every step in its own pod. Week 4 is about what that pod may do —
and about the two ways a security control gets to be fictional.

**The first fiction: limits nobody applies.** The tool descriptors had carried
`cpu`, `memory` and `image` since Week 1, and nothing read them. The engine built
each attempt's limits from the step's timeout and attempt budget alone, so every
Job was created with no resource limits at all: the descriptors said `500m`, and
the pods got the node. Copying the descriptor's numbers into the pod spec would
have fixed the symptom and encoded something worse — *the thing being executed
decides how much of the machine it gets* — one week before a language model
starts writing the thing being executed.

So [`internal/policy`](internal/policy) sits between them. A descriptor is a
**request**; the operator's configuration is the **grant**; the effective
envelope is `min(request, ceiling)`, defaulted, never widened. Resources clamp
silently — a portable descriptor meeting a smaller cluster should run smaller,
not fail — while capabilities refuse loudly, with a classified terminal error
naming the switch to flip. It is resolved on **every attempt**, not once at
submission, so a policy tightened while a step sits behind a retry backoff binds
the very next attempt rather than only new work.

**The second fiction: a policy the CNI ignores.** kind's default CNI accepts
`NetworkPolicy` objects and enforces nothing. `kubectl get networkpolicy` lists
them, `describe` prints the rules, no event is emitted, and every packet flows.
Every observable signal reports success.

That is why the sandbox has three independent mechanisms rather than one:

```
deploy/kubernetes/20-networkpolicy.yaml   default-deny for every task pod;
                                          egress only for runmesh.io/network=allow,
                                          to 0.0.0.0/0 EXCEPT RFC 1918 and 169.254/16
deploy/kind/verify-networkpolicy.sh       four real pods, real packets. The fourth
                                          probe is the one worth having: an allowed
                                          pod must NOT reach the cluster's own API
internal/k8s/sandbox_test.go              reads the shipped manifests and fails if
                                          the label Go sets stops matching the
                                          selector the YAML uses — a drift that
                                          leaves pods matching NO policy, which
                                          Kubernetes treats as unrestricted
cmd/task/http.go                          the tool refuses to connect to any
                                          non-public address itself, checked in the
                                          dialer AFTER DNS resolution, so rebinding
                                          does not help and every redirect is covered
```

`169.254.169.254` is why the `except` list exists at all: on EC2, GCE and Azure
it hands out the node's cloud credentials to anything that asks, over plain HTTP,
with no authentication. An egress rule of `0.0.0.0/0` with no exclusions passes
every other check and is a direct path from "the model chose to fetch a URL" to
the cluster's cloud identity.

**And `python_execute`, which is the point of all of it.** The Python image ships
an ordinary, unrestricted CPython: real builtins, real imports, the whole
standard library. There is no stripped `__builtins__`, no import audit, no AST
filter — every one of those has been bypassed publicly, and a filter that
*mostly* works is worse than none, because it manufactures the belief that the
code was vetted. The sandbox is the pod, and the interpreter inside it is assumed
hostile ([ADR 0009](docs/decisions/0009-the-sandbox-is-the-pod.md)). A test fails
if somebody later adds a content filter to the parameter validator.

The pod spec is asserted field by field, because every one of these is a line
that can be deleted without breaking anything else, and the effect of deleting it
is invisible until it matters:

```
runAsNonRoot + runAsUser 65532        set at BOTH pod and container level: a
                                      container securityContext REPLACES the pod's
readOnlyRootFilesystem                nothing to modify, nothing to persist
capabilities: drop [ALL]              a list that ages vs. one that does not
allowPrivilegeEscalation: false       no setuid path out
seccompProfile: RuntimeDefault        omitting it means Unconfined
cpu / memory / ephemeral-storage      the third takes a NODE down, not a pod:
                                      container logs count towards it
emptyDir at /tmp with a sizeLimit     the only writable path, and it is quota'd
automountServiceAccountToken: false   plus a ServiceAccount bound to nothing
enableServiceLinks: false             no free map of the namespace in the env
```

### Testing

```bash
make test            # everything that needs no database
make test-pg         # starts PostgreSQL, then the whole suite under -race
./task.ps1 test-pg   # the same, on Windows
```

The PostgreSQL cases **skip** when `RUNMESH_TEST_DATABASE_URL` is unset, so
`go test ./...` still works on a laptop with no database. CI is precisely where
that skip would be a lie, so CI always provides one — and a test fails the build
if it ever stops doing so. A conformance suite that silently skips is worse than
no conformance suite: it reports green for a store nobody ran.

There is **no `time.Sleep` anywhere in the suite** — enforced by the same
`go/parser` walk that guards production code. Time is injected, so a test that
exercises a 30-second step timeout finishes in microseconds. Runtime tests
assert on the **event stream**, which is simultaneously the assertion and the
synchronisation primitive: one spurious extra attempt fails the test, where a
final-status check would pass.

### What the review found

The Week-1 code was put through an adversarial review — six independent
reviewers over the codebase, then three skeptics per finding, each trying to
*refute* it. Twenty-three findings, thirteen survived. The two that mattered:

- **The abandon timer was re-armed by every heartbeat.** The cancel flag is
  sticky, so each heartbeat after a cancellation reported the same
  cancellation and reset the timer. With the *shipped defaults* — heartbeat 5s,
  grace 10s — it was reset five seconds before it could ever fire, so a tool
  ignoring its context held a worker for the life of the process and every
  shutdown ended in `ErrDrainIncomplete`. Its regression test hangs without
  the fix.
- **Fail-fast did not apply to lease expiry.** The trigger lived in `Finish`,
  so a step driven to FAILED by the reconciler instead — a dead worker
  exhausting its budget — raised no cancel flag. Its dependents sat QUEUED for
  ever and the job never terminalised. The fix moved it into the one function
  every writer ends in, so it is a property of the transition rather than of
  one call site.

Both regressions live in the shared store suite, and both now hold the
PostgreSQL store too — the fail-fast one twice over, since the policy that fixes
it is the code `pgstore` calls rather than a rule it re-implements.

The highest-value test here is [`internal/storetest`](internal/storetest): an
**exported conformance suite** that `memstore` passed in Week 1 and `pgstore`
passes now. Every design claims its store interface is database-ready; this one
made the claim executable, and then ran it — 34 cases against a real server,
including 32 concurrent claimers taking 500 steps and each step coming out
exactly once.

Every Week-1 case passed against PostgreSQL unmodified, and not one line of
`engine.Store` or `httpapi.Store` changed to accommodate it. That is the actual
test of whether a consumer-declared interface was designed or merely described.

**One case was added, and it is the more interesting half of the story.** The
suite did *not* catch that `pgstore.ListJobs` returned jobs without their steps,
because every existing case checked ids and counts — while `GET /api/v1/jobs`
renders each job's steps in full. Two stores, two different response bodies from
one endpoint. It was found by reading the API layer against the new store, and
the fix went into the *suite* rather than into `pgstore`'s own tests, so both
implementations are now held to it, including the one that was already right.
A conformance suite is a living contract, not an artefact of the week it was
written.

> **Windows note.** The race detector needs cgo and a **64-bit** C compiler.
> A 32-bit MinGW on `PATH` fails with *"64-bit mode not compiled in"*.
> `./task.ps1 race` finds a usable toolchain automatically — it looks in
> `C:\msys64\mingw64in` and two other usual places; otherwise
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
part of, an abandon grace longer than the lease it is protecting, a task uid of
`0` that `runAsNonRoot` would have every pod rejected for at admission.

The execution policy is configuration too, and its defaults are closed: no
network for any tool, `250m` / `128Mi` / `64Mi` for a tool that declares nothing,
and no `python_execute` at all until an image is named for it. A tool registered
with no image would pass plan validation and then fail every attempt; absent is
the honest answer, and the 400 says `unknown_tool` and lists what *is* available.

What is deliberately **not** configurable: `runAsNonRoot`, the read-only root
filesystem, the dropped capabilities, the seccomp profile. A security control an
operator can switch off with an environment variable is a security control that
will be switched off with an environment variable.

---

## Repository

```
cmd/server/          the only wiring in the codebase, and a whole-binary test
internal/
  runmesh/           domain: states, plans, jobs, leases, events. stdlib only
  clock/             the only source of time, plus the purity test enforcing it
  jobstate/          the transition policy BOTH stores call. pure, stdlib only
  memstore/          in-memory store, for development and for fast tests
  pgstore/           the durable store: SKIP LOCKED queue, migrations, fencing
  storetest/         the conformance suite both stores pass, unmodified
  engine/            dispatcher, worker pool, reconciler, and the pure policy
  tools/             the plugin boundary and the Executor seam for Kubernetes
  k8s/               one Job per attempt; the only package that imports client-go
  policy/            the execution policy: the tool asks, the operator grants
  httpapi/           net/http only; its own narrower view of the store
  config/            every knob, validated at boot
cmd/task/            the container tool contract, implemented. No RunMesh imports
migrations/          the schema, embedded in the binary
deploy/
  docker/            the server, task and Python-sandbox images
  kind/              the local cluster, and the script that proves the sandbox
  kubernetes/        namespaces, least-privilege RBAC, the NetworkPolicies
docs/
  architecture/      diagrams and the map of where the engineering is
  decisions/         ADRs for the choices with real alternatives
```

Fifteen packages, two binaries, two direct dependencies — and `client-go` reaches
exactly one of the fifteen.

### Design records

- [0001 — Execution is at-least-once](docs/decisions/0001-execution-is-at-least-once.md)
- [0002 — PostgreSQL is the queue; Redis is deferred](docs/decisions/0002-postgresql-is-the-queue.md)
- [0003 — Execute tasks as Kubernetes Jobs, not raw Pods](docs/decisions/0003-kubernetes-jobs-not-pods.md)
- [0004 — Standard-library `net/http`, zero third-party dependencies](docs/decisions/0004-standard-library-http.md)
- [0005 — The store owns every state transition](docs/decisions/0005-store-owns-transitions.md)
- [0006 — Readiness is a predicate, not a state](docs/decisions/0006-readiness-is-derived.md)
- [0007 — One dependency: the PostgreSQL driver](docs/decisions/0007-one-dependency-the-postgres-driver.md)
- [0008 — The job row is the lock](docs/decisions/0008-the-job-row-is-the-lock.md)
- [0009 — The sandbox is the pod, not the interpreter](docs/decisions/0009-the-sandbox-is-the-pod.md)
- [0010 — The plan asks; the operator grants](docs/decisions/0010-the-plan-asks-the-operator-grants.md)

---

## What this is not

RunMesh is not trying to replace Temporal, Celery, Ray, BullMQ or Kubernetes
itself. It is a focused implementation of the execution layer an AI agent
needs, built to be understood end to end and defended in detail: controlled
tool execution, dependency orchestration, failure recovery, and observability
of *why* something took as long as it did.
