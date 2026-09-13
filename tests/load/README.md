# Load tests

Four [k6](https://k6.io) scripts, run against a RunMesh server you started
yourself. They are not part of `go test`: they need a listener, they take
minutes, and a load test that runs on every commit is a load test that gets
deleted the first time CI is slow.

| Script | Question it answers |
| --- | --- |
| `smoke.js` | Is everything wired up — the key's scopes, the tool, the submit, read and scrape paths — before ten minutes go into a real run? It is `make load`'s default, and its thresholds are about correctness, not capacity. |
| `submit-throughput.js` | How many plans a second can `POST /api/v1/jobs` admit, and where does the latency curve bend? |
| `mixed-read-write.js` | Do the dashboard's reads and an agent's writes interfere, and which side degrades first? |
| `soak.js` | Does anything drift — goroutines, queue depth, dropped events, the heap — over a long run at a rate the server is comfortably inside? |

`lib/runmesh.js` holds the shared plan shape, the request helpers and the
custom metrics, so every script measures the same thing and their numbers
can be compared with each other.

## Running one

```bash
# 1. a server, with a key that carries the scopes the scripts use.
#    metrics.read is only needed by the soak, which scrapes /api/v1/metrics.
RUNMESH_HTTP_ADDR=127.0.0.1:8099 \
RUNMESH_API_KEYS='load:jobs.read+jobs.write+metrics.read=0123456789abcdef0123456789abcdef' \
RUNMESH_ENABLE_TEST_TOOLS=true \
RUNMESH_WORKERS=8 RUNMESH_CLAIM_BATCH=8 \
RUNMESH_MAX_QUEUE_DEPTH=20000 \
RUNMESH_LOG_LEVEL=error \
go run ./cmd/server

# 2. the load
cd tests/load
RUNMESH_BASE_URL=http://127.0.0.1:8099 \
RUNMESH_API_KEY=0123456789abcdef0123456789abcdef \
k6 run submit-throughput.js
```

Every script calls `preflight()` in `setup()`, so a missing key, an unreachable
server or a key without the right scope fails the run immediately with a
sentence saying which — rather than producing a full run of 401s and a
suspiciously fast summary.

## Configuration

Everything is an environment variable. Nothing is hard-coded, and there is no
host in any committed file.

| Variable | Default | Used by |
| --- | --- | --- |
| `RUNMESH_BASE_URL` | `http://127.0.0.1:8080` | all |
| `RUNMESH_API_KEY` | *(required)* | all |
| `RUNMESH_LOAD_TOOL` | `echo` | all — `sleep` to make the worker pool the bottleneck instead of the API |
| `RUNMESH_LOAD_SLEEP` | `20ms` | all, when the tool is `sleep` |
| `RUNMESH_LOAD_RATE` | `200` | submit-throughput — peak submissions/second |
| `RUNMESH_LOAD_VUS` | `100` | submit-throughput — pre-allocated VUs |
| `RUNMESH_LOAD_STAGE` | `30s` | submit-throughput — duration of each of the four ramp stages |
| `RUNMESH_LOAD_WRITE_RATE` | `30` | mixed — submissions/second |
| `RUNMESH_LOAD_READ_RATE` | `300` | mixed — dashboard polls/second |
| `RUNMESH_LOAD_SEED_JOBS` | `20` | mixed — jobs created in `setup()` for the readers to read |
| `RUNMESH_LOAD_DURATION` | `1m` | mixed |
| `RUNMESH_LOAD_SOAK_RATE` | `20` | soak — submissions/second |
| `RUNMESH_LOAD_SOAK_DURATION` | `10m` | soak |
| `RUNMESH_LOAD_SAMPLE_SECONDS` | `5` | soak — how often `/api/v1/metrics` is scraped |

## These scripts fail; they do not report

Every claim each script makes is a k6 **threshold** with `abortOnFail`, so a
regression is a non-zero exit code rather than a number somebody was supposed
to notice. k6 exits **99** when a threshold is crossed.

Two of the thresholds are worth explaining, because both look backwards.

**`runmesh_backpressure_rate` has an upper bound, not a lower one.** A 429 from
admission control is the server *working*: `RUNMESH_MAX_QUEUE_DEPTH` is a
deliberate ceiling, and refusing beyond it is the behaviour the design wants.
So refusals are excluded from the error rate — but a run in which most
submissions were refused measured a full queue rather than throughput, and
reading its latency numbers as a throughput result would be wrong. Hence
`rate<0.5`.

**`dropped_iterations` fails the test, and it is the *test* that failed.** The
throughput and mixed scripts use open-loop arrival-rate executors, which send
requests at a rate rather than as fast as replies come back — the distinction
matters, because a closed-loop VU loop cannot overload a server and reports a
flattering latency all the way down. When k6 cannot keep to the rate with the
VUs it was given, it drops iterations, and the rate axis of the whole run
becomes fiction. That has to fail loudly, or the summary quietly says `400/s`
about a run that never reached it.

## What a run does to the server

Nothing cleans up. The jobs these scripts submit stay in the store: they *are*
the load, and removing them would need a `DELETE` endpoint that does not exist
and should not exist so that a benchmark can tidy up. Start each measured run
against a fresh store.

That is not just hygiene. With the in-memory store, admission control's queue
depth is computed by walking every job and every step, so **submission latency
degrades as the store fills** — on the machine in `docs/benchmarks`, p95 went
from 2.1 ms against an empty store to 5.9 ms against the same server after
9,249 jobs, without anything else changing. Comparing two runs against stores
of different sizes measures the fixture, not the change.
