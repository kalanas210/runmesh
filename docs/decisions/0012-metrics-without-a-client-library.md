# ADR 0012 — Metrics without a client library

**Status:** Accepted · Week 6 · applies the test set by
[ADR 0007](0007-one-dependency-the-postgres-driver.md), which in turn superseded
the Week-1 scope of [ADR 0004](0004-standard-library-http.md)

## Context

Week 6 needs `/metrics`. The obvious move is
`github.com/prometheus/client_golang`, and it would be a fifth direct
requirement in a `go.mod` whose direct block currently holds four: the pgx
driver and the three Kubernetes client modules.

ADR 0007 did not say "dependencies are bad". It set a two-part test and worked
through it out loud, and this is the second time that test is being applied.

**Part one: what can only this library do?** For pgx the answer was concrete —
speak the PostgreSQL wire protocol: a startup handshake, SCRAM-SHA-256, the
extended query protocol, something like 1,500 lines of it. For
`client_golang` the honest answer is: serialise a text format specified in
about one page. Four line shapes (`# HELP`, `# TYPE`, `name{k="v"} value`, and
the `_bucket`/`_sum`/`_count` convention), three escape sequences, one
`Content-Type: text/plain; version=0.0.4`. Underneath it, three data
structures that are roughly 250 lines of `atomic.Uint64`. There is no protocol
negotiation, no cryptography, no version handshake, and nothing on the other
end of a socket that has to agree with us about anything except those four line
shapes.

**Part two: can it be contained?** pgx earned its place because containment
was provable: a blank import in one file, and one string in
`sql.Open("pgx", dsn)`. No query, no type and no error path in the codebase
mentions pgx, so swapping it is changing one string rather than auditing every
scan. `client_golang` cannot be contained that way. `prometheus.Counter`,
`prometheus.Labels`, `*prometheus.Registry` and `promhttp.Handler` would appear
in `internal/metrics`, in whatever type `engine.Deps` accepts, in
`internal/httpapi`'s middleware chain, and in `cmd/server/run.go`. Every
instrumented call site in the engine would name a third-party type. That is
precisely the objection 0007 raised when it rejected `pgxpool` — "it would put
pgx types in every query and every scan" — and it is a worse version of it,
because the engine is the part of this codebase the whole project exists to
demonstrate.

Counting the transitive modules out loud, because 0007 says a budget that only
counts the line you typed is not a budget: `client_golang` brings
`prometheus/client_model`, `prometheus/common`, `prometheus/procfs`,
`beorn7/perks` and `cespare/xxhash/v2`. `procfs` is a Linux-only `/proc`
parser; on the Windows machine this is developed on it does nothing at all.

## Decision

Hand-roll `internal/metrics` in pure standard library: counters, gauges and
explicit-bucket histograms on atomics, plus a Prometheus text-exposition 0.0.4
writer. Nothing is added to `go.mod`.

`internal/metrics` is the only package in the repository that knows the
exposition format exists. Every consumer sees an interface it declared itself —
`engine.Observer` (nine methods, in `internal/engine/observer.go`),
`httpapi.Gatherer` (one method), `httpapi.HTTPObserver` (one method),
`metrics.Runtime` and `metrics.StoreSource` (declared by `internal/metrics`,
over the engine and the store). Nothing is registered in an `init()` and nothing
reaches for a default registerer: `promauto` plus the package-level default
registry is ruled out on its own by the rule written at `cmd/server/run.go`
("run is the ONLY wiring in the codebase … no package reaches out for a
global"), before the dependency question is reached at all.

`GET /api/v1/metrics` is in the route table like every other endpoint, under a
new `config.ScopeMetricsRead`, rather than mounted outside it by
`promhttp.Handler` — which would bypass the scope decision that
`internal/httpapi/api.go`'s table exists to make auditable.

### The correctness risk, named

Hand-writing an exposition format has five specific hazards, and the reason
this is defensible rather than reckless is that all five are testable.

1. **Family grouping.** Prometheus rejects a whole scrape if a metric name
   carries two `# HELP` lines, or if one family's samples are interleaved with
   another's. The registry therefore renders families in lexical order with
   every sample of a family contiguous, and refuses a duplicate name at
   registration.
2. **Cumulative buckets.** The classic hand-rolled bug is incrementing only the
   matching bucket and emitting it raw, after which `histogram_quantile`
   returns nonsense. Per-bucket counts are stored and running-summed at render,
   ascending — the same shape `client_golang` uses.
3. **`+Inf` and `_count`.** A histogram without an `le="+Inf"` bucket is
   invalid, and `+Inf` must equal `_count`. `_count` is not stored at all: it is
   emitted *as* the `+Inf` cumulative, so the two cannot disagree by
   construction. That is an invariant `client_golang` has to maintain and this
   design gets for free.
4. **Float formatting of `le`.** A boundary that formats differently between
   two scrapes silently retires one series and starts another.
   `strconv.AppendFloat(b, v, 'g', -1, 64)`, with `+Inf` spelled literally, is
   the only deterministic round-trip, and the boundary slices are compile-time
   constants so they cannot drift.
5. **Escaping and name validity.** Label values need `\`, `"` and newline
   escaped; metric names must match `[a-zA-Z_][a-zA-Z0-9_]*`. Every label value
   in this process comes from a closed vocabulary already stringified by house
   code — `Stop.String()`, `State.String()`, the `runmesh.Code*` constants, the
   matched route pattern from `routeOf` — so none of these can actually occur.
   The writer escapes anyway, because the guarantee worth having is "this
   writer cannot emit an unparseable line", not "our current labels happen to be
   safe".

Three tests retire them: a round-trip test that renders, re-parses with a small
test-only parser and asserts the parse reproduces the registry's typed values; a
golden file driven by a `*clock.Fake` so the bytes are stable and a human has
read them once against the spec; and a lint test that reimplements the handful
of `promlint` rules that matter here — except the name rules also run at
*registration*, so an illegal name is a panic in `run()` at boot rather than a
CI failure later.

### Cardinality: the axis where hand-rolling is genuinely better

`client_golang`'s `GetMetricWithLabelValues` mints a new series for any string
it is ever handed. That is the standard production cardinality explosion, and
the library offers no structural defence against it.

Every label vocabulary in this process is closed at wiring time: tool names
from `tools.Registry.Names()`, states from `runmesh.AllStates`, stop reasons
from the five `Stop` values, error codes from the `runmesh.Code*` constants,
routes from `httpapi.RoutePatterns()`. So `metrics.Label` is declared as a name
*plus its permitted values*. A value outside the set resolves to a shared
`other` child and increments `runmesh_metrics_label_rejected_total{metric=…}` —
a cardinality mistake becomes a metric instead of an OOM. That in turn makes the
maximum number of time series this binary can ever export a constant computable
at boot, which `TestSeriesBudget` asserts against `Registry.SeriesUpperBound()`.

"The upper bound on our series count is a number in a test" is a stronger
statement than any `client_golang` deployment can make, and it is only possible
because the vocabulary is closed — a property of this domain, not of the
library. The one genuinely open axis is tool-supplied error codes, since a tool
may emit its own; that is exactly what `other` is for.

### Concurrency

The house rule is written at `internal/memstore/subscribe.go`: observability
must never apply backpressure to execution. It bites harder here than for the
event fan-out, because an observer call from `settle` runs on a goroutine that
is *holding a pool capacity token*. A slow implementation does not make metrics
slow; it shrinks the worker pool.

So: `Counter` is an `atomic.Uint64`; `Gauge` is an `atomic.Int64` (every gauge
in this runtime counts things, which avoids the `math.Float64bits` CAS dance
entirely); `Histogram.Observe` is a linear scan over at most twelve `float64`
boundaries — faster and more branch-predictable than binary search at that size
— then one CAS loop on the sum bits and one atomic add into the bucket. Zero
allocations, zero locks, asserted with `testing.AllocsPerRun` rather than left
as prose. The `Vec` map sits behind an `RWMutex` taken in read mode only, and
pre-materialised children make the common path a read lock plus a map lookup.

What this gives up against `client_golang`'s hot/cold generation swap is that
`_sum` and `_count` can skew by a single observation within one scrape instant.
That is real. It is stated here, the writes are ordered so `_count` is the
conservative one, and it is accepted deliberately — the alternative is a mutex
on the settle path, which is exactly the trade this codebase says it will not
make.

## Consequences

- **The Go runtime collector is mostly recovered, cheaply.** `runtime/metrics`
  gives `/gc/cycles/total:gc-cycles`, `/gc/heap/allocs:bytes`,
  `/memory/classes/heap/objects:bytes` and `/memory/classes/total:bytes` as
  exact `uint64` scalars, and `runtime.NumGoroutine()` is one line.
  `/cpu/classes/gc/total:cpu-seconds` is exact too but it is a **`float64`**,
  and that distinction is not pedantry — it cost us a metric. It was first
  registered through `CounterFunc(func() uint64)`, so `uint64(…)` floored a
  reading that on a healthy process never reaches one whole second, and
  `go_cpu_gc_seconds_total` published a flat `0` under a help string promising
  CPU seconds. The registry now has a float-valued counter path for cumulative
  durations (`Registry.CounterFloatFunc`); the exposition `TYPE` is still
  `counter`, because Prometheus has never required a counter's samples to be
  integers, only that they do not decrease.
  `TestSecondsCountersAreNotBridgedThroughIntegers` is the rule that keeps the
  mistake from coming back. `go_goroutines` matters more here than
  anywhere else, because the engine's package doc *fixes* the goroutine census
  at 2 + Workers + one per in-flight step — so that gauge is a live assertion of
  a stated design invariant. `/sched/latencies:seconds` is re-bucketed onto our
  own boundaries; exporting Go's own buckets verbatim would add 50–250 series,
  which is why `client_golang` re-buckets it too. It is also the one
  runtime metric that is a POINTER value, which `runtime/metrics` documents as
  sharing storage that a later `Read` reuses — so the collector deep-copies it
  while still holding the source's mutex rather than returning the runtime's own
  slices to the renderer. What is not recovered is the
  full `go_memstats_*` breakdown and GC pauses as a summary.
- **The process collector is the honest loss.** `process_cpu_seconds_total`,
  `process_resident_memory_bytes` and `process_open_fds` come from
  `/proc/self/{stat,status,fd}` via `prometheus/procfs` — roughly sixty lines to
  reimplement on Linux, a no-op on Windows and macOS in `client_golang` too, and
  already carried, better, by cAdvisor's
  `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`
  in the Kubernetes target. We ship `process_start_time_seconds`, which is
  trivial, standard-library-only and the one Prometheus itself uses for restart
  detection. We deliberately *refuse* to name a Go-runtime approximation
  `process_cpu_seconds_total` or `process_resident_memory_bytes`:
  `/memory/classes/total:bytes` is not RSS, and naming an approximation after
  the real thing is worse than omitting it.
- **OpenMetrics is lost.** It is this text format plus `# EOF`, `# UNIT` and
  the counter family/sample naming rule — about thirty lines behind `Accept`
  negotiation. Prometheus falls back to text 0.0.4 unless configured otherwise,
  so the loss is small and cheap to reverse.
- **Exemplars are genuinely lost and genuinely irrecoverable.** There is no
  OpenTelemetry here, no trace ids, and nothing that propagates
  `X-Request-ID` into the engine, so an exemplar today would have nothing to
  point at. See the reopening condition below.
- **`testutil` is the strongest argument for the library, and it is conceded.**
  `testutil.ToFloat64` is replaced more cheaply than it is lost — a hand-rolled
  `Counter` exposes `Value() uint64` and a test reads the typed value directly,
  in-package, with no gathering at all. `CollectAndCompare` is replaced by the
  golden file, which this codebase's fixture style already favours.
  `GatherAndLint`/`promlint` is the real loss, and moving those rules to
  registration time is strictly better than keeping them in a test. What does
  not come back is the free confidence of a serialiser that has been scraped
  billions of times; the mitigation is the round-trip test plus pointing one
  real Prometheus at `/metrics` and reading its own scrape-error log before
  anyone trusts a dashboard.
- **A hand-rolled registry cannot receive a third-party collector.**
  `prometheus.Registerer` is the interface every instrumented library registers
  against. There is no such library in this module today — the pool is
  `database/sql`, whose `DB.Stats()` is a struct you would read yourself
  anyway — so the cost is deferred, and the exit is cheap: rendering is one
  `WriteTo`, and serving `client_golang`'s gathered output concatenated from the
  same handler is about ten lines. The decision is reversible, which is the last
  thing that makes it defensible.
- **Our `go_*` names are not `client_golang`'s `go_memstats_*` names**, so an
  off-the-shelf Grafana "Go Processes" dashboard shows empty panels. That is a
  real, small cost, and the mitigation is to ship our own dashboard rather than
  to rename an approximation after something it is not.
- **A malformed body fails the scrape wholesale.** Prometheus rejects the whole
  document and sets `up=0`, so one ordering or duplicate-series bug takes out
  every panel at once rather than one. This is the residual the library would
  have absorbed, and it is why the round-trip and golden tests exist.

## Reopening condition

If a future week adds OpenTelemetry tracing, exemplars acquire something to
point at and this decision should be retaken. That is the one event that
changes the arithmetic; a fifth dependency for a one-page serialisation format
is not.

## Alternatives considered

- **`client_golang` used directly — `promauto`, the default registry,
  `promhttp.Handler`.** Ruled out twice over before the dependency question is
  reached. `promauto` registers into a package-level default registry, which
  `cmd/server/run.go` forbids in so many words. And `promhttp.Handler` would be
  mounted outside `routes()`, bypassing the scope decision the route table
  exists to make auditable and that `TestEveryRouteStatesItsScope` walks.
- **`client_golang` confined to `internal/metrics` behind house-declared
  interfaces.** The real alternative, and it still fails 0007's containment
  test. The confinement is an illusion: `prometheus.Registerer` *is* the point
  of the library, so a registry nobody else can register into throws away the
  only thing you are paying for — while still paying five transitive modules,
  one of which is a Linux-only `/proc` parser. It also solves the cheap part of
  the job and not the expensive part: the serialiser is 250 lines against a
  one-page spec, whereas choosing names, base units, bucket boundaries and a
  cardinality policy is the actual work, and the library helps with none of it.
  You would also still hand-write the bridge into `writeError`/`Recover`,
  because `promhttp.Handler` writes its own status codes.
- **The OpenTelemetry metrics SDK plus its Prometheus exporter.** Strictly worse
  on every axis that decided this: many more modules, a metric model that
  introduces delta-versus-cumulative temporality and Views as concepts the
  operator must learn, and a global `MeterProvider` idiom that collides with the
  no-globals rule. It makes sense only as part of adopting OTel tracing
  wholesale, which is the reopening condition above and not this decision.
- **Feed metrics from the existing event stream — `Subscribe` to
  `runmesh.Event` and count.** A counter fed by a lossy channel is a counter
  that lies, and `rate()` over it is unfixable after the fact. Both stores'
  publish does a non-blocking send and DROPS when a subscriber is slow;
  memstore's rings evict; and pgstore's `global_seq` allocation order is
  documented as not being commit order, so a tailing consumer can permanently
  miss an event. The event stream is the durable, ordered, replayable product
  surface. Metrics are a lossy in-process side channel. Conflating them makes
  the durable thing carry a load it was not designed for and makes the lossy
  thing silently wrong.
- **Skip Prometheus: widen `engine.Stats()` and serve JSON at
  `/api/v1/stats`.** No histograms, so no p95 attempt duration and no way to
  tell a slow tool from a slow store — which is the single question Week 6
  exists to answer. No per-tool dimension without inventing a nested JSON shape
  every consumer has to aggregate itself, and it pushes `rate()` arithmetic onto
  whatever scrapes it. `engine.Stats()` stays exactly as it is, feeding
  `GET /api/v1/ready`: it is a readiness snapshot, not a telemetry surface.
- **A second listener on a dedicated metrics port.** It buys network-level
  isolation that this deployment achieves with a scope instead, and it costs a
  second `net.Listener` with its own `Serve` goroutine that must be stopped in
  the right order inside `shutdown()` before the hard-exit watchdog fires. It
  would also lose, on that port, everything the main chain already provides:
  `RequestID`, the panic barrier, the request log line and the low-cardinality
  route label. Two new `RUNMESH_*` knobs, two new `Validate()` cross-checks, two
  new `.env.example` lines, and a fresh port-collision hazard.
- **Register `/metrics` as `scopePublic`, like the probes.** The probes are
  public because a load balancer often cannot hold a credential, and that
  justification is written out in the route table. It does not extend to
  Prometheus, which has an `authorization` stanza and a `credentials_file` in
  `scrape_configs`. A public `/metrics` exports queue depth, job counts, worker
  counts and per-tool failure codes to anyone who can reach the port: free
  reconnaissance.
- **Scope `/metrics` to the existing `jobs.read`.** Workable, and the fallback
  if a reviewer wants zero new scopes — but it hands a scrape token the
  authority to list every job and read every event body, tool results included.
  This codebase has already made exactly this argument once, splitting
  `jobs.cancel` out of `jobs.write` because "submitting work and stopping
  somebody else's work are different authorities". Reading aggregate counters
  and reading job payloads are different authorities by the same reasoning.
- **A seventh middleware for HTTP metrics.** It would have to re-wrap the
  `ResponseWriter` to capture the status, duplicating `statusRecorder`, and it
  could only see the status through a type assertion on the recorder `Logger`
  has already installed. `Logger`'s deferred func already holds the status, the
  bytes written, `clk.Since(start)` and `routeOf(ctx)`. Emitting the metric
  there is four lines, adds no machinery, and makes it structurally impossible
  for the log line and the metric to disagree about the same request.
- **Add a method to `engine.Store` so stores report their own latency.** A
  four-file change — memstore, pgstore, `storetest.Store` and every double —
  against an interface whose own doc treats "no edit to this file" as the proof
  the abstraction was designed. A decorator over the existing seven methods, in
  `cmd/server` where both store interfaces are already named, gets the same
  latency data with no edit to the contract.
