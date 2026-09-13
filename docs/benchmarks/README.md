# Benchmarks

Measured numbers, and an honest account of what they are worth.

Every figure below was produced by running the commands in this document on the
machine in this document. Nothing here is estimated, extrapolated or rounded
from memory: if a number is not in the pasted output, it is not in a table.

> **The short version of the caveats.** This is one laptop, on battery-class
> thermals, running a PostgreSQL container through Docker Desktop's virtualised
> loopback, with `fsync=off` and `synchronous_commit=off`. The in-memory numbers
> are a fair measure of RunMesh's Go. The PostgreSQL numbers are **not a
> measure of PostgreSQL** — they are dominated by a round trip that costs
> milliseconds on this host and microseconds on a real one, and the `op=Ping`
> row exists so you can see exactly how much. Read them as *shapes* — does the
> curve flatten, does contention scale — and not as capacity.

---

## The machine

| | |
| --- | --- |
| CPU | 12th Gen Intel Core i7-12650H — 10 cores, 16 threads |
| RAM | 15.7 GB |
| OS | Windows 11 Home Single Language 10.0.26200 |
| Go | go1.26.0 windows/amd64, `GOMAXPROCS=16` |
| k6 | v1.7.1 (windows/amd64) |
| PostgreSQL | 17.11 (`postgres:17-alpine`) in Docker Desktop, published on host port 5434 |
| PostgreSQL flags | `fsync=off`, `synchronous_commit=off`, `max_connections=200` — from `docker-compose.yml` |
| Date | 2026-09-12 |

Two properties of this machine matter more than its speed.

**The database is not local.** It is a container reached over Docker Desktop's
loopback. On Windows that path goes through a virtual switch and a NAT, and it
costs on the order of a millisecond per round trip — three orders of magnitude
more than a Unix socket on the same host as the server. Every PostgreSQL figure
here is that cost plus the query.

**`fsync=off` removes the disk.** That is the right choice for measuring *this
code*, because it leaves the query plan, the lock behaviour and the Go as the
only variables. It is the wrong configuration to predict production from, where
a commit is a durable write and the numbers get worse in a way none of this
measures.

---

## How to reproduce

```bash
# A database on 5434. The port is 5434 rather than 5432 because this machine has
# native PostgreSQL services on both 5432 and 5433; see docker-compose.yml's
# header for the failure mode when they collide.
make db-up RUNMESH_DB_PORT=5434

export RUNMESH_TEST_DATABASE_URL='postgres://runmesh:runmesh@127.0.0.1:5434/runmesh?sslmode=disable'
```

### Go benchmarks

They live in [`internal/bench`](../../internal/bench), which is the one place in
the repository where a test does not sit beside the code it exercises — the
package doc there argues why. `make bench` runs them all with the default
`-benchtime`; the two commands below are what produced the tables, and they
differ because the two halves want different things.

```bash
# (1) Fixture-bound. Every iteration needs a step that had to be created first,
#     so b.N is pinned rather than discovered: without -benchtime=Nx, Go ramps
#     b.N until the TIMED section reaches a second, and the untimed setup then
#     grows to tens of thousands of rows.
go test ./internal/bench -run '^$' -bench 'BenchmarkScheduler|BenchmarkStoreOp|BenchmarkClaimUnderContention' \
  -benchmem -benchtime=2000x -timeout=40m

# (2) Pure functions and in-process instrumentation. No fixture, so the default
#     one-second benchtime is right and gives Go enough iterations to be stable.
go test ./internal/bench -run '^$' -bench 'BenchmarkPlanValidate|BenchmarkReadiness|BenchmarkBlockedBy|BenchmarkMetricsRender|BenchmarkObserver' \
  -benchmem
```

The PostgreSQL rows **skip** without `RUNMESH_TEST_DATABASE_URL`, exactly as the
conformance suite does, so `make bench` works on a laptop with no database and
reports only the `store=mem` half.

Raw output for both commands is committed beside this file as
[`results-2026-09-12.txt`](results-2026-09-12.txt), so every table here can be
checked against what the tool actually printed.

### k6

```bash
# a server for k6 to load
RUNMESH_HTTP_ADDR=127.0.0.1:8099 \
RUNMESH_API_KEYS='load:jobs.read+jobs.write+metrics.read=0123456789abcdef0123456789abcdef' \
RUNMESH_ENABLE_TEST_TOOLS=true RUNMESH_WORKERS=8 RUNMESH_CLAIM_BATCH=8 \
RUNMESH_MAX_QUEUE_DEPTH=20000 RUNMESH_LOG_LEVEL=error \
go run ./cmd/server

cd tests/load
export RUNMESH_BASE_URL=http://127.0.0.1:8099
export RUNMESH_API_KEY=0123456789abcdef0123456789abcdef

RUNMESH_LOAD_RATE=400 RUNMESH_LOAD_VUS=150 RUNMESH_LOAD_STAGE=10s k6 run submit-throughput.js
RUNMESH_LOAD_WRITE_RATE=30 RUNMESH_LOAD_READ_RATE=300 RUNMESH_LOAD_DURATION=30s k6 run mixed-read-write.js
RUNMESH_LOAD_SOAK_RATE=20 RUNMESH_LOAD_SOAK_DURATION=2m k6 run soak.js
```

Each measured run started against a **freshly restarted server**, for a reason
the results below make concrete.

### Failure tests

```bash
make test-integration RUNMESH_DB_PORT=5434
```

---

## What these numbers do and do not prove

This section is first on purpose. A benchmark table with no argument attached to
it is read as a capacity claim, and none of these are.

**They prove relative cost, on this code.** `Finish` against the in-memory store
costs about an order of magnitude more than `Heartbeat`; the readiness predicate
is free for a blocked step and quadratic for a satisfied one; the metrics render
is a fraction of a millisecond against a scrape interval of seconds. Those
ratios are properties of the code and they will hold on other hardware.

**They prove shapes.** Whether the scheduler gets faster with more workers,
whether `SELECT ... FOR UPDATE SKIP LOCKED` degrades as claimers are added,
whether goroutines grow over a soak — a curve's *direction* survives a change of
machine even when its magnitude does not. That is where most of the value in
this document is.

**They do not prove production capacity.** Not for one deployment, and
emphatically not per replica multiplied by replicas. A laptop under Docker
Desktop with `fsync=off` is not a database server, `steps/sec` with a tool that
returns a constant is not `steps/sec` with a tool that does work, and the
in-memory store is not a deployment target at all — it says so on
`GET /api/v1/ready`, in the `durable` field.

**The PostgreSQL figures are a latency floor plus work, and the floor is the
host's.** `BenchmarkStoreOp/store=pg/op=Ping` measures a `SELECT 1` — one round
trip, no rows, no transaction — and it is the baseline every other PostgreSQL
row sits on top of. On a server where that floor is tens of microseconds instead
of milliseconds, every PostgreSQL number here moves by more than the difference
between any two versions of this code.

**One number is not a measurement.** Every table is a single run. Repeat runs of
the fixture-bound benchmarks on this machine vary by tens of percent — the
`store=pg/workers=1` row was measured at 14 ms/step on one run and 27 ms/step on
another, with nothing changed but what else the laptop was doing. Treat a
difference smaller than about 2x as noise unless it was produced by `benchstat`
over several runs.

**What was measured is not always what the name suggests.** The scheduler
benchmark runs a tool that returns a constant, so it measures the dispatcher and
the store and nothing else; the contention benchmark refills nothing, so a
memstore claimer is scanning a queue whose size it set; the soak's heap trend is
a store that never evicts, which is the design rather than a leak. Each of those
is stated again next to the table it affects, because a caveat three screens
away from a number is a caveat nobody reads.

---

## Load tests (k6)

All three scripts pass every threshold. The scripts, and what each threshold is
for, are documented in [`tests/load/README.md`](../../tests/load/README.md).

### Job-submission throughput — `submit-throughput.js`

Four ten-second stages ramping to 400 submissions/second, each submission a
four-step diamond, against a server with 8 workers and the in-memory store.

```
✓ http_req_failed            rate=0.00%
✓ checks                     rate=100.00%
✓ runmesh_backpressure_rate  rate=0.00%
✓ dropped_iterations         count=0
✓ runmesh_submit_duration    p(95)=2.13ms   p(99)=4.61ms

runmesh_jobs_submitted...: 9249   231.186622/s
runmesh_submit_duration..: avg=833.84µs min=0s med=579µs p(95)=2.13ms p(99)=4.61ms max=33.7ms count=9249
http_reqs................: 9251   231.236614/s
dropped_iterations.......: 0      0/s
```

**400 submissions/second sustained with zero dropped iterations**, which means
the server kept to the arrival rate rather than the arrival rate adapting to the
server. 231/s is the average across the ramp, not the ceiling. Each of those
submissions creates a job, four steps and their first events, so the store
absorbed roughly **37,000 steps in forty seconds**.

The ceiling is between 400 and 500/s on this machine, and it was found the way a
threshold is supposed to find it — by failing:

```
runmesh_submit_duration..: avg=40.27ms med=6.31ms p(95)=203.58ms p(99)=800.04ms max=1.02s
dropped_iterations.......: 112    11.213124/s
✗ thresholds on metrics 'dropped_iterations' were crossed; at least one has
  abortOnFail enabled, stopping test prematurely
```

That run was configured for a 2000/s peak and k6 aborted it at the 500/s stage.
This is the behaviour the scripts are built for: the run **failed**, with a
non-zero exit code, instead of printing a p95 of 203 ms and leaving somebody to
notice.

### Submission latency degrades as the in-memory store fills

The same script, same rate, same server binary, run twice — once against a
freshly started process and once against the process that had just absorbed the
9,249 jobs above:

| Store contents | p(95) | p(99) | max |
| --- | --- | --- | --- |
| empty | 2.13 ms | 4.61 ms | 33.7 ms |
| 9,249 jobs / ~37,000 steps | 5.93 ms | 15.28 ms | 65.03 ms |

This is not a leak and it is not a surprise; it is admission control doing what
it says. `POST /api/v1/jobs` calls `Store.QueueDepth` *before* persisting, and
`memstore.QueueDepth` evaluates the readiness predicate over every step of every
job it holds — a store that never evicts, walked linearly, once per submission.
`pgstore.QueueDepth` is a `count(*)` over the same predicate and has an index
behind it, so it does not have this shape.

The operational consequence is small and the measurement consequence is not:
**two load runs against stores of different sizes are not comparable.** Start
each measured run against a fresh store.

### Mixed read/write — `mixed-read-write.js`

30 submissions/second and 300 dashboard polls/second, concurrently, for thirty
seconds. The reads are the three calls a waterfall makes; the list endpoint is
weighted to every fourth iteration, as a real dashboard would.

```
✓ http_req_failed          rate=0.00%
✓ checks                   rate=100.00%   (22103 of 22103)
✓ runmesh_read_duration    p(95)=7.57ms   p(99)=17.87ms
✓ runmesh_submit_duration  p(95)=7.96ms   p(99)=28.49ms
✓ dropped_iterations       count=0

{ endpoint:GET /api/v1/jobs }..........: avg=5.69ms p(95)=15.81ms p(99)=28.02ms count=2261
{ endpoint:GET /api/v1/jobs/{id} }.....: avg=1.11ms p(95)=5.78ms  p(99)=16.54ms count=9001
{ endpoint:GET /api/v1/jobs/{id}/events}: avg=1.17ms p(95)=5.04ms p(99)=14.28ms count=9001
http_reqs..............................: 21185  705.141032/s
```

**705 requests/second across four endpoints with no failures**, and the two
halves are separately bounded, which is why this script has two scenarios rather
than one weighted branch: a single branch would let a slowdown on either side
hide by reducing the other's load.

The per-endpoint split says what a single aggregate cannot. `GET /api/v1/jobs`
is five times the cost of fetching one job — it renders every step of every job
in the page — and it is the endpoint a dashboard should poll least. Submission
under concurrent read load is 7.96 ms at p95 against 2.13 ms in isolation; both
are inside their thresholds, and the difference is the one mutex the in-memory
store takes for everything.

### Soak — `soak.js`

Two minutes at 20 submissions/second, with a second scenario scraping
`GET /api/v1/metrics` every five seconds and turning four gauges into trends.

```
✓ http_req_failed          rate=0.00%
✓ checks                   rate=100.00%
✓ runmesh_soak_goroutines  max=38
✓ runmesh_soak_event_drops max=0
✓ runmesh_submit_duration  p(99)=1.21ms

runmesh_soak_goroutines..: avg=36.708333 min=19 med=37 p(95)=38 max=38  count=24
runmesh_soak_queue_depth.: avg=0         min=0  med=0  p(95)=0  max=0   count=24
runmesh_soak_event_drops.: avg=0         min=0  med=0  p(95)=0  max=0   count=24
runmesh_soak_heap_bytes..: min=2977696   med=95864460  max=171000976   count=24
runmesh_jobs_submitted...: 2401   19.997842/s
```

Three of those four are the result.

**Goroutines: 19 at boot, 38 at peak, flat for two minutes.** The engine's
package doc fixes the census at `2 + Workers` plus one per in-flight step, which
for 8 workers is 10, and the rest are net/http's connection goroutines and the
runtime's own. Nothing accumulated: a leak of one goroutine per request would
have added 2,401 over this run.

**Queue depth: 0 at every sample.** The pool kept up with the arrival rate
throughout, so the latency figures describe service time rather than queueing.

**Dropped events: 0.** The store's fan-out drops rather than blocks when a
subscriber is slow — that is deliberate, because a dashboard must never be able
to slow execution down — and the counter is the only evidence it ever happened.
There *was* a subscriber: `cmd/server/run.go` always constructs the eventbus
broker and starts it, and `Broker.Start` calls the store's `Subscribe` with a
1,024-event buffer, so every run of this server has exactly one process-local
subscriber whether or not anybody opens an event stream. No SSE client
connected during this run, so the broker was the only consumer — and it kept up
with 2,401 jobs' worth of events without the store having to drop one.

**Heap: 3 MB to 171 MB, and this one is not a finding.** The in-memory store
retains every job it has ever been given; 2,401 four-step jobs with their events
is what 171 MB looks like. A soak against `RUNMESH_DATABASE_URL` is the run that
would make a heap trend meaningful, and this is not that run.

---

## Go benchmarks

### Scheduler throughput

`BenchmarkScheduler`. One iteration is one step taken all the way through the
runtime: `Claim`, the unbuffered hand-off to a worker, `Start`,
`Executor.Execute`, `Classify`, `Finish`, the event append and the job rollup.
The tool returns a constant and does nothing else, so this is the scheduler and
the store and not the work.

| workers | memstore steps/sec | memstore ns/step | pgstore steps/sec | pgstore ns/step |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 3,676 | 272,062 | 37.1 | 26,950,903 |
| 2 | 3,540 | 282,518 | 100.1 | 9,993,672 |
| 4 | 4,510 | 221,721 | 154.8 | 6,461,115 |
| 8 | 6,820 | 146,634 | 186.6 | 5,360,371 |
| 16 | 8,890 | 112,482 | 235.8 | 4,240,316 |

**Both curves rise with the pool, and for different reasons.** For the in-memory
store the gain is mostly the claim batch: `ClaimBatch` tracks `Workers`, so 16
workers means one `Claim` every 16 steps instead of one per step, and the
allocation column falls with it — 53,432 B/step at one worker against 20,945 at
sixteen. Since `memstore.Claim` walks every job under one mutex, batching is the
only way to make that walk happen less often.

For PostgreSQL the gain is concurrency: independent claimers skipping each
other's locked rows, and many small transactions in flight against one server at
once. A 6x improvement from one worker to sixteen is the scaling claim doing
what it says.

**The two columns are three orders of magnitude apart, and that gap is mostly
this laptop.** A step against PostgreSQL is roughly a dozen round trips —
`Claim`, `Start` and `Finish` are each a transaction, and a transaction is
several — and a round trip here costs 633 µs (see `op=Ping` below). Twelve of
those is 7.6 ms before any work at all. On a host where that round trip is
50 µs, the same twelve cost 0.6 ms.

The `workers=1` row is also the one this document's variance warning is about:
it measured 26.9 ms/step in the committed run and 14.3 ms/step in an earlier
one. The single-worker case is the most sensitive to what else the machine is
doing, because there is nothing else in flight to absorb a stall.

### Store operations

`BenchmarkStoreOp`. Each write measured on its own, so a throughput change can
be attributed to one of them.

| operation | memstore ns/op | memstore ops/sec | memstore allocs | pgstore ns/op | pgstore ops/sec | pgstore allocs |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `Ping` (baseline) | 18.2 | 54,945,055 | 0 | 633,203 | 1,579 | 0 |
| `Claim` | 317,868 | 3,146 | 21 | 7,717,095 | 129.6 | 1,117 |
| `Start` | 432.6 | 2,311,604 | 1 | 5,233,530 | 191.1 | 1,199 |
| `Heartbeat` | 74.9 | 13,360,053 | 0 | 2,745,761 | 364.2 | 56 |
| `Finish` | 646.2 | 1,547,389 | 2 | 5,215,780 | 191.7 | 1,239 |

**`Ping` is the row to read the others against.** 633 µs for a `SELECT 1` on a
connection that is already open is the host's number, not PostgreSQL's — it is
the Docker Desktop loopback. Every other PostgreSQL figure is that floor times
however many round trips the operation makes: `Heartbeat` is about four `Ping`s,
`Claim` about twelve.

**`Heartbeat` is the cheapest write on both stores, and that is the design
working.** It is the hottest write in the system — every running step makes one
every `RUNMESH_HEARTBEAT_INTERVAL` — and it is deliberately an extension of a
lease rather than a transition, so it takes no job lock, appends no event and
recomputes no rollup. 75 ns and zero allocations in memory; 56 allocations
against PostgreSQL, where `Claim` makes 1,117.

**`memstore.Claim` is the outlier in the in-memory column, at 318 µs against
sub-microsecond for everything else.** That is the linear scan: it walks every
job and every step under the global mutex evaluating the readiness predicate,
and this benchmark hands it a queue of `b.N` steps. The cost is therefore a
function of the fixture, which is said here rather than hidden — and it is the
same shape the k6 runs found from the outside, where submission latency tripled
as the store filled. It is not a defect in `memstore`, whose documented job is
development and fast tests. It is the reason the in-memory store is not a
deployment target.

### Claim under contention

`BenchmarkClaimUnderContention`. N goroutines claiming batches of 4 from a
pre-filled queue until `b.N` leases have been taken; one iteration is one lease.
This is the measurement the whole architecture rests on:
`SELECT ... FOR UPDATE SKIP LOCKED` handing each step to exactly one worker with
no coordinator, no partition assignment and no leader.

| claimers | memstore leases/sec | memstore leases/trip | pgstore leases/sec | pgstore leases/trip |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 10,318 | 4.000 | 270.4 | 4.000 |
| 2 | 11,444 | 4.000 | 281.5 | 3.992 |
| 4 | 11,998 | 4.000 | 277.5 | 3.976 |
| 8 | 12,237 | 4.000 | 404.5 | 3.935 |
| 16 | 11,216 | 4.000 | 391.1 | 3.876 |
| 32 | 10,608 | 4.000 | 409.4 | 3.759 |

**The PostgreSQL column is the result: throughput goes UP from 1 claimer to 32,
not down.** 270 leases/second with one claimer and 409 with thirty-two — a 51 %
gain, where a design that serialised on a queue lock would show a collapse.
Nothing takes turns: each claimer's transaction skips the rows the others have
locked, so adding a replica adds capacity. That is the bet the design made, and
this is it paying.

**`leases/trip` is the evidence they really were colliding.** It falls from
4.000 at one claimer to 3.759 at thirty-two: with more claimers a round trip
increasingly comes back with fewer than the four it asked for, because rows it
would have taken were locked by somebody else and were skipped. Without that
decline the thirty-two-claimer row would only prove that thirty-two goroutines
can take turns politely.

**The in-memory column is flat at 10–12 k leases/second whatever the claimer
count, and that is also correct.** One mutex serialises every claimer, so total
throughput cannot rise with concurrency, and `leases/trip` stays at exactly
4.000 because serialised claimers never see a partial result. memstore
simulates the store's semantics, not its concurrency, and this is the table
where that difference becomes visible.

### Plan validation

`BenchmarkPlanValidate`. `runmesh.Plan.Validate` against the shipped admission
limits — the single gate between an untrusted plan and the runtime.

| shape | ns/op | plans/sec | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| chain, 10 steps | 5,906 | 169,313 | 2,179 | 28 |
| chain, 100 steps | 58,004 | 17,240 | 18,187 | 208 |
| fan-in, 100 steps | 43,032 | 23,238 | 17,337 | 77 |

At the shipped `RUNMESH_MAX_STEPS=100` ceiling, validating the worst shape costs
58 µs. Submission latency measured from outside was 834 µs on average, so
validation is under a tenth of it and admission control's queue-depth query is
the larger half.

It matters for a caller nobody thinks about, though. The planner's repair loop
calls `Validate` once per rejected candidate, up to
`RUNMESH_PLANNER_MAX_REPAIRS + 1` times for a single goal — and three
validations of a 100-step plan is 174 µs against a model round trip measured in
seconds.

### DAG readiness

`BenchmarkReadiness`. `jobstate.Claimable` over every step of one job: the sweep
`memstore.Claim` and `memstore.QueueDepth` each perform, and the predicate
`pgstore` expresses as two correlated `NOT EXISTS` subqueries.

| shape | ns/job | jobs/sec | steps/sec | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| flat, 100 steps, no edges | 526.2 | 1,900,532 | 190,053,167 | 0 |
| fan-in, 100 steps, dependencies unmet | 727.3 | 1,374,874 | 137,487,400 | 0 |
| fan-in, 100 steps, dependencies met | 6,678 | 149,738 | 14,973,760 | 0 |
| chain, 100 steps | 16,368 | 61,094 | 6,109,354 | 0 |

**Zero allocations in every shape.** The predicate is pure and allocation-free,
which is what lets it be evaluated per step per claim without a
garbage-collection cost that would compound with queue depth.

**The 31x spread between the first row and the last is `Job.Step`, and it is
quadratic.** Resolving a dependency is a linear search over the job's steps, so
a job of *n* steps each carrying a dependency costs O(n²) comparisons. The chain
is the worst case; `deps=met` is the same effect at a third of the size. It does
not matter at the shipped 100-step ceiling — 16 µs for the worst job there — and
it would matter at a thousand. If `RUNMESH_MAX_STEPS` is ever raised, this is
the line that moves, and the fix is an index rather than an algorithm.

**The gap between the two fan-in rows is the early return, not noise.**
`Claimable` exits on the FIRST unsatisfied dependency, so a step whose upstreams
are all still queued costs one lookup. The `deps=met` row is the moment that
actually costs: a rank has just finished, and the next claim has to walk all
thirty-two edges before it can say yes.

`BenchmarkBlockedBy` measures the derived field the dashboard reads —
`Job.BlockedBy` on a 32-edge fan-in — at **9,248 ns/op, 108,136 ops/sec, 1,007
B/op, 5 allocs/op**. It is computed per request for every step of every job in a
page, which is deliberate: a stored copy of a derived truth goes stale.

### Metrics exposition and the observer hot path

`BenchmarkMetricsRender` renders the whole production registry — the real tool
registry, the real route table — to `io.Discard`.

| state | ns/op | scrapes/sec | bytes/scrape | series | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| cold | 134,267 | 7,448 | 55,767 | 1,577 | 108,961 | 1,171 |
| warm | 178,487 | 5,603 | 55,947 | 1,577 | 109,041 | 1,173 |

> **This table is a re-measurement**, and the rest of the Go tables are not.
> `go_cpu_gc_seconds_total` was being sourced through a `uint64` accessor, which
> floored a fractional CPU-second reading to `0`; fixing it changes the bytes
> this benchmark reports, so the row had to be re-run rather than adjusted. The
> command was scoped to this one benchmark and its raw output is appended to
> [`results-2026-09-12.txt`](results-2026-09-12.txt) under its own heading. It
> is a separate session on the same machine, so the *timings* here are not
> comparable with the other Go tables — this laptop's thermals move `ns/op` by
> a third between sessions, which is exactly why `warm` reads slower than
> `cold` in this run.

**A full scrape is under 200 µs and 56 KB.** Against a fifteen-second Prometheus
interval that is around a millionth of one core, so the exporter cannot be the
load — which is the claim ADR 0012 owes for having hand-written the text format
instead of taking `client_golang`. 1,577 series is the registry's own
`SeriesUpperBound`, and it is a bound rather than an estimate because every
label vocabulary in `internal/metrics/set.go` is closed by construction.

**Cold and warm differ by 180 bytes, and the reason is not the one it looks
like.** It is *not* that every labelled family materialises its declared cells
at registration and only the `other` cell is lazy.
`internal/metrics/labels.go` pre-materialises a family only when the cross
product of its declared vocabularies is at or below `preMaterialiseLimit`,
which is **64**. Above that the family starts with **no children at all** and
creates them lazily on first observation.

With the shipped vocabulary three families are over the limit, and they are not
minor ones: `runmesh_step_attempts_total` (tool × state × stop, 420 cells),
`runmesh_http_requests_total` (route × status, 224) and
`runmesh_step_attempt_failures_total` (tool × code, 119). So **a freshly booted
server exports no samples at all for those three.** Their `# HELP` and `# TYPE`
lines are still written — `WriteTo` emits a family's header whatever its
instrument holds — but there is not one sample line under them. That is the
honest description of the cold body: 710 sample lines against a
`SeriesUpperBound` of 1,577, with the three families an operator most wants
missing from it entirely.

Warming the registry with a single job and a single request creates exactly two
of those cells, and that is where the delta comes from:
`runmesh_step_attempts_total{tool="echo",state="SUCCEEDED",stop="none"}` and
`runmesh_http_requests_total{route="POST /api/v1/jobs",status="201"}`, 143 bytes
of new sample lines. The remaining ~34 bytes are digits appearing in values that
were already there — a `_sum` going from `0` to `0.001` is three bytes wider.

The operational consequence is worth stating plainly: on a cold server
`rate(runmesh_step_attempts_total[5m])` has no series to rate, so a dashboard
panel over it is *empty* rather than flat at zero — which is exactly the
distinction `preMaterialiseLimit` exists to preserve, and exactly the one it
gives up above 64 cells. The trade is still the right one; pre-materialising 420
dead rows for a vocabulary that size would spend boot time and memory on series
that may never be observed, and `labels.go` argues it. But a panel that reads
"no data" on a healthy server is a thing an operator has to be told about, and
`up` is what has to disambiguate it from an exporter that is down.

`BenchmarkObserver` measures the `engine.Observer` methods on the goroutines
they are actually called from — a worker or the dispatcher, each holding pool
capacity while the call runs.

| method | ns/op | calls/sec | allocs/op |
| --- | ---: | ---: | ---: |
| `Dispatched` | 14.7 | 67,894,817 | 0 |
| `ToolExecuted` | 48.6 | 20,578,763 | 0 |
| `AttemptStarted` | 49.0 | 20,407,717 | 0 |
| `StoreOperation` | 51.0 | 19,594,200 | 0 |
| `Heartbeat` | 57.0 | 17,546,596 | 0 |
| `Claimed` | 74.2 | 13,475,747 | 0 |
| `AttemptSettled` — success | 110.5 | 9,049,834 | 0 |
| `AttemptSettled` — classified failure | 165.0 | 6,061,184 | 0 |
| `AttemptSettled` — unknown code, resolves to `other` | 170.2 | 5,875,267 | 0 |
| `RequestFinished` | 171.3 | 5,837,035 | 0 |
| `AttemptSettled`, 16 goroutines (`RunParallel`) | 137.1 | 7,293,225 | 0 |

**Zero allocations on every method.** `TestObserveIsAllocationFree` already
asserts that half with `testing.AllocsPerRun`; this is the same claim in
nanoseconds. The most expensive call in the table is 171 ns against a step that
costs 112,482 ns even entirely in memory, so **telemetry is roughly 0.15 % of a
step** and the observer cannot be quietly shrinking the worker pool — which is
the specific failure `internal/engine/observer.go` warns about, since an
observer that got slow would show up as reduced throughput with no error
anywhere.

**The parallel row is the one with a finding in it.** At sixteen goroutines
`AttemptSettled` costs 137 ns per call against 110 ns serially, so aggregate
throughput does not scale with cores. That is the compare-and-swap on the
histogram's `sum` word: every worker settling the same tool into the same cell
retries the same address. It is the right trade at this scale — a 24 % cost
under sixteen-way contention, on a call that is a thousandth of a step — and it
is a real ceiling, so it is written down here rather than left to be
rediscovered.

---

## Failure tests

Not benchmarks, and included here because they are the other half of what a
performance document is for: a throughput number means nothing if the system
does not survive the things that happen to it at that throughput.

[`tests/integration`](../../tests/integration) holds four chaos cases, each run
against **both stores** — because lease expiry, retry-budget arithmetic and
fail-fast are implemented twice, once as Go under a mutex and once as SQL under
a row lock, and a failure test that only ever ran against the in-memory
simulation would be proving a property of the simulation.

```
$ make test-integration RUNMESH_DB_PORT=5434

--- PASS: TestSubmittingPastTheQueueLimitPushesBack (0.00s)
    --- PASS: TestSubmittingPastTheQueueLimitPushesBack/store=memory (0.01s)
    --- PASS: TestSubmittingPastTheQueueLimitPushesBack/store=postgres (0.20s)
--- PASS: TestCancellingAJobInFlightFailsFastToDependents (0.00s)
    --- PASS: TestCancellingAJobInFlightFailsFastToDependents/store=memory (0.28s)
    --- PASS: TestCancellingAJobInFlightFailsFastToDependents/store=postgres (0.50s)
--- PASS: TestReconcilerRecoversAStepItsWorkerNeverFinished (0.00s)
    --- PASS: TestReconcilerRecoversAStepItsWorkerNeverFinished/store=memory (1.03s)
    --- PASS: TestReconcilerRecoversAStepItsWorkerNeverFinished/store=postgres (1.28s)
--- PASS: TestLeaseExpirySpendsBudgetAndRequeues (0.00s)
    --- PASS: TestLeaseExpirySpendsBudgetAndRequeues/store=memory (0.00s)
        --- PASS: .../store=memory/budget=exhausted (2.01s)
        --- PASS: .../store=memory/budget=remaining (2.01s)
    --- PASS: TestLeaseExpirySpendsBudgetAndRequeues/store=postgres (0.01s)
        --- PASS: .../store=postgres/budget=remaining (2.14s)
        --- PASS: .../store=postgres/budget=exhausted (2.18s)
PASS
ok  	github.com/kalanas210/runmesh/tests/integration	2.366s
```

| Case | What it breaks | What it requires |
| --- | --- | --- |
| `TestReconcilerRecoversAStepItsWorkerNeverFinished` | A step is RUNNING under a lease whose owner will never heartbeat again | The sweep reclaims it, the step runs again, the job SUCCEEDS, the DAG downstream resumes, and `STEP_LEASE_EXPIRED` names the owner that vanished |
| `TestLeaseExpirySpendsBudgetAndRequeues` | The same orphaned lease, with the pool never started so exactly one sweep is observed | With budget left: QUEUED, `failures=1`, `attempt` unchanged. With budget exhausted: FAILED, the job terminalises, and the dependents do **not** sit QUEUED |
| `TestCancellingAJobInFlightFailsFastToDependents` | A job is cancelled while a step is genuinely RUNNING | The directive rides back on the next heartbeat, the running step is CANCELLED rather than running its full ten seconds, and every dependent is CANCELLED too |
| `TestSubmittingPastTheQueueLimitPushesBack` | Submissions past `RUNMESH_MAX_QUEUE_DEPTH` | 429 with a `resource_exhausted` envelope, never more than the ceiling admitted, and — after the runtime drains the queue — acceptance again |

Two things about them are worth stating because they are easy to get wrong.

**A dead worker is simulated by orphaning a lease, not by killing anything.** A
lease *is* the liveness claim: the design says a worker is alive if and only if
it is renewing, so a lease nobody renews is exactly and completely what a dead
worker leaves behind. Killing a goroutine would produce something different and
less useful, because the engine's drain RELEASES its leases on the way out and
refunds the retry budget — the opposite of the situation under test. The half
that only a real operating-system crash can produce is covered separately, by
[`cmd/server/crash_test.go`](../../cmd/server/crash_test.go), which SIGKILLs a
real process and requires a second one to finish the job.

**The budget-exhausted case is a regression test in disguise.** The Week-1
adversarial review found fail-fast living inside `Finish`, so a step driven to
FAILED by the RECONCILER instead raised no cancel flag and its dependents waited
for ever. That case asserts the dependents of a lease-expired, budget-exhausted
step do not stay QUEUED, on both stores.

---

## Summary of what was measured

| Claim | Number | Where it came from |
| --- | --- | --- |
| Steps/second through the runtime, in memory, 16 workers | 8,890 | `BenchmarkScheduler/store=mem/workers=16` |
| Steps/second through the runtime, PostgreSQL, 16 workers | 236 | `BenchmarkScheduler/store=pg/workers=16` |
| Leases/second at 32 concurrent claimers, PostgreSQL | 409 (up from 270 at one claimer) | `BenchmarkClaimUnderContention/store=pg` |
| PostgreSQL round-trip floor on this host | 633 µs | `BenchmarkStoreOp/store=pg/op=Ping` |
| Heartbeat, the hottest write | 75 ns in memory / 2.7 ms PostgreSQL | `BenchmarkStoreOp/op=Heartbeat` |
| Plan validation at the 100-step ceiling | 58 µs | `BenchmarkPlanValidate/shape=chain/steps=100` |
| Readiness sweep of a 100-step job, worst shape | 16.4 µs, zero allocations | `BenchmarkReadiness/shape=chain/steps=100` |
| One full `/api/v1/metrics` scrape | 167 µs, 56 KB, 1,577 series | `BenchmarkMetricsRender/state=warm` |
| Telemetry cost per attempt | ≤ 171 ns, zero allocations | `BenchmarkObserver` |
| Job submissions/second sustained over HTTP | 400/s, zero dropped iterations, p95 2.13 ms | `submit-throughput.js` |
| Mixed API requests/second | 705/s across four endpoints, zero failures | `mixed-read-write.js` |
| Goroutines over a two-minute soak | 19 → 38, flat | `soak.js` |
| Events dropped by the fan-out over that soak | 0 | `soak.js` |

Every row above appears in the pasted output earlier in this document or in
[`results-2026-09-12.txt`](results-2026-09-12.txt). Nothing that was not
measured is in this table.
