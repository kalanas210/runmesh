# ADR 0013 — Server-Sent Events, not WebSockets

**Status:** Accepted · Week 6 · overrides the plan document, which says
"WebSocket updates" · applies the dependency test set by
[ADR 0007](0007-one-dependency-the-postgres-driver.md), for the third time
after [ADR 0012](0012-metrics-without-a-client-library.md)

## Context

Week 6 needs a live execution timeline: a dashboard that draws a waterfall of
steps as they are scheduled, started, retried and finished. The plan document
names WebSockets, and that is the choice being overridden here, so it gets the
same treatment 0007 gave the dependency budget and 0012 gave the metrics
client.

Three facts about the existing codebase decide almost all of this.

**The data flow is one-way.** What a browser receives is
`runmesh.Event` values appended to a durable, ordered per-job timeline. What a
browser sends is nothing: the two things a dashboard wants to do — cancel a job,
submit a plan — are already `POST` routes with idempotency keys, scopes and a
202 contract. A bidirectional transport buys a channel this design has no use
for.

**The resume cursor already exists, and it is the same number SSE uses.**
`runmesh.Event.Seq` is documented at `internal/runmesh/event.go` as per-job,
1-based and **gap-free**, assigned under the job lock; `?after=` on
`GET /api/v1/jobs/{id}/events` is exclusive on exactly that field. SSE's `id:`
frame field, the `Last-Event-ID` request header a client sends on reconnect, and
that cursor are one value. The resume protocol is not designed here, it is
inherited.

**Both stores' `Subscribe` is process-local.** `pgstore`'s own doc comment says
so: it carries events written by *this* process. A design that served a stream
from the in-process fan-out alone would show each dashboard, on a two-replica
deployment behind a load balancer, the subset of the timeline that happened to
land on its replica — a plausible, wrong waterfall. That is worse than polling,
and it is the failure most designs of this shape quietly accept and then
document as a caveat.

## Decision

`GET /api/v1/jobs/{id}/stream`, Server-Sent Events over plain `net/http`, in
the route table under `config.ScopeJobsRead`, authenticated by the unchanged
`Authorization: Bearer` header.

Three parts, and the third is the one that makes the first two honest.

### The transport is SSE and costs nothing

`text/event-stream`, `http.NewResponseController`, and a `for`/`select`. Zero
new bytes in `go.mod`. Frames are `id:`/`event:`/`data:` triples terminated by a
blank line:

| frame | payload | meaning |
| --- | --- | --- |
| `snapshot` | `{job, events, next_after, truncated, oldest_seq}` | the job and its timeline so far, in one frame |
| `event` | one `runmesh.Event` | byte-identical to an element of the polling endpoint's `events` array |
| `resync` | `{reason, oldest_seq}` | your cursor predates retained history |
| `end` | `{state}` | the job is terminal and the timeline is complete — stop reconnecting |
| `bye` | `{reason}` | the server is going away — reconnect with backoff from your last id |
| `error` | the standard `{"error":{…}}` envelope | a failure after the header was sent |
| `: ping` | — | heartbeat; a bare comment, so it carries no id and cannot move a cursor |

The `snapshot` frame replaces "GET the job, then open a stream, and hope nothing
happened in between" and deletes that race class rather than documenting it.
The `event` payload is deliberately the *same* JSON the polling endpoint
already returns, which is the fallback plan written into the wire format: if the
stream ever has to be abandoned, the browser's `Event` type and the reducer that
folds events into waterfall rows are unchanged and the swap is a transport
change rather than a rewrite.

### The credential is the one that already exists

`EventSource` cannot set request headers, but that is only binding if you use
`EventSource`. `fetch()` can set `Authorization`, and
`response.body.getReader()` plus a `TextDecoderStream` is a frame reader in
about thirty lines. The Next.js app proxies the stream through its own route
handler, which holds the scoped key server-side and pipes `upstream.body`
straight through, so the key never reaches a browser, the browser talks
same-origin and no CORS middleware is added outside `Auth`. The cost is stated
plainly below.

### Correctness is the durable timeline; the bus is an accelerator

The handler runs **one** select over five cases — client gone, draining, a live
event, the heartbeat tick, the poll tick — following the single-select design
`internal/engine/worker.go` argues for at length. The poll leg reads
`Store.JobEvents` from the cursor every `RUNMESH_STREAM_POLL_INTERVAL`
(default 1s). `internal/eventbus` is a process-local broker that delivers the
same events sooner; a live event is emitted immediately **and resets the poll
ticker**, so on the replica that wrote the event the observed latency is
sub-millisecond and the poll never fires, while a replica that did not write it
picks it up within a second.

Remove the bus entirely and the stream still delivers every event, one poll
later. That inversion — durable source of truth, optional accelerator — is what
lets this ship with no "events from this replica only" caveat and no
single-replica-per-stream limitation.

A slow consumer is handled by exploiting the gap-free cursor rather than by
choosing between three bad options. The bus drops on a full buffer and counts
it, because observability may never apply backpressure to execution — but a
drop is *detectable*: the next live event's `Seq` exceeding `lastSeq+1` says
exactly what was missed, and one `JobEvents` read repairs it before the frame is
emitted. A drop becomes a catch-up query, not a client-visible hole and not a
disconnect. The one case that cannot be repaired is the in-memory ring evicting
history the client never received; that gets an explicit `resync` frame, because
rendering a timeline with a silent hole is the failure worth a frame of its own.

### Three blockers, fixed at their source

The package was built entirely around short request/response cycles, and
streaming through it was blocked by three concrete things. None is worked around
in the handler.

1. **`statusRecorder` could not be unwrapped.** `Logger` installs it on every
   request, and it satisfied neither `http.Flusher` nor `http.Hijacker` and had
   no `Unwrap`, so `http.NewResponseController` returned
   `http.ErrNotSupported` and a stream buffered every frame until the handler
   returned — which for a stream is for ever. One method,
   `Unwrap() http.ResponseWriter`, makes `Flush`, `SetWriteDeadline` and
   `Hijack` all reachable. A hand-written `Flush()` would have been
   insufficient, because clearing the write deadline is the next blocker.
2. **`RUNMESH_WRITE_TIMEOUT` is absolute, not idle.** At its 30-second default
   every stream is severed on a fixed schedule; because clients reconnect, that
   looks like "it works" in a two-minute manual test and arrives in production
   as a reconnect storm. The handler clears the deadline for its own connection
   via `ResponseController.SetWriteDeadline(time.Time{})`. Setting the
   server-wide value to 0 was rejected: it removes slow-client protection from
   all eleven other routes to fix one.
3. **`Recover` spliced JSON into a half-written body.** It wrote its envelope
   unconditionally, which on a response whose header is already out produces a
   superfluous-`WriteHeader` warning through `srv.ErrorLog` and puts a raw
   `{"error":…}` object into a `text/event-stream` body — not a frame, so the
   browser's parser sees a malformed chunk and the client's own reconnect walks
   straight back into the same panic with no record of why. `Recover` now falls
   silent once `sr.status != 0` and the handler owns its own failure reporting
   from its first flush onward, emitting an `error` frame from its own recover
   barrier, the way the engine's panic barriers still produce an outcome.

And a fourth that would have shipped silently: `http.Server.Shutdown` waits for
connections to go **idle**, and an SSE connection never does. A handler that
ignored the drain would hold `Shutdown` for the full `RUNMESH_SHUTDOWN_GRACE`,
return `DeadlineExceeded`, and turn that into `code = 1` — every deploy with a
dashboard attached exiting non-zero. The handler selects on `a.draining`,
writes `bye`, and returns.

## Consequences

- **A 1s floor of one indexed query per open stream on an idle job.** With
  `RUNMESH_STREAM_MAX=64` that is at most 64 `WHERE job_id=$1 AND seq > $2`
  lookups a second against the existing `UNIQUE (job_id, seq)` index. It does
  not go to zero the way pure push would. A live event resets the ticker, so an
  active stream on the writing replica pays nothing; a wall of idle dashboards
  on *terminal* jobs is the shape to watch, which is why the stream closes
  itself with `end` when its job is terminal.
- **A second `// clock:allow:` in the repository.** The per-frame
  `SetWriteDeadline` is a *kernel* deadline compared against wall-clock time by
  the netpoller; handing it the fake clock's 2026-09-05 epoch would expire every
  write on a real socket instantly. The fake clock drives this stream's protocol
  timing — the heartbeat and the poll, both through `a.clock` — and the socket
  deadline stays what the operating system understands. It is bounded rather
  than absent because a TCP peer that is dead but not closed, with a full socket
  buffer, is otherwise an unbounded handler goroutine. The alternative — a
  second, explicitly-named system clock field for kernel deadlines only — is
  more honest about the two kinds of time but adds a field nothing else would
  use.
- **`Logger`'s line is deferred until the handler returns**, so an hour-long
  stream emits nothing for an hour and then one line with
  `duration_ms=3600000`. The HTTP request-duration histogram added in
  [ADR 0012](0012-metrics-without-a-client-library.md) is fed from that same
  deferred func, so it would be dominated by how long people leave browser tabs
  open — a graph that is simply wrong, with nothing failing to say so. The
  streaming routes are therefore excluded from the histogram by name
  (`httpapi.StreamingRoutePatterns`) and the count is exported as an
  active-streams gauge pulled from the API's own admission counter instead, so
  the gauge and the cap cannot disagree.
- **`cmd/server/run.go`'s `store` union gains `Subscribe`.** Safe only because
  both concrete stores already implement it, so no store file and no
  `storetest/suite.go` change. Anything further the stream later wants — a
  NOTIFY-backed `Subscribe`, a `Snapshot` — becomes a four-file change across
  memstore, pgstore, the conformance suite and every double.
- **The broker is a new goroutine, and where it is stopped is the whole safety
  argument.** `bus.Close()` sits inside `shutdown()` *after* `eng.Shutdown` — so
  no event is produced with nobody to consume it — and *before* `store.Close()`
  — so the pump is not left reading a channel the store closed underneath it.
  Putting it in a `defer` next to the store's is the easy mistake and gets the
  order exactly backwards.
- **A closed subscription means shutdown, never end-of-job.** The handler writes
  `bye`, not `end`. Reading it the other way would make a rolling restart look
  to every dashboard like every in-flight job had completed.
- **The Next.js proxy is a confused-deputy surface.** It holds a scoped RunMesh
  key and will stream any job id it is asked for unless it applies its own
  session check first. That check is the only thing between a signed-out visitor
  and a whole job timeline, and it lives in a file the Go test suite cannot see.
- **No store-wide feed ships.** A dashboard index page showing many jobs at once
  still polls. See the `GlobalSeq` alternative below for what has to be solved
  first.
- **`TestScopeMatrix` does not auto-discover routes.** A scoped stream route in
  `routes()` without the matching `calls` entry is registered, live, and
  completely untested for authorisation — the exact failure that test's own
  comment says it exists to prevent. The entry is added with the route.

## Reopening condition

LISTEN/NOTIFY is the eventual upgrade and it drops in *here*, as a second
producer behind `internal/eventbus` feeding the same fan-out, with no change to
the handler, the wire format or the browser client. Its cost is a dedicated
connection outside the pgx pool. When it lands, the poll interval becomes a
safety net that rarely fires rather than the correctness mechanism it is today,
and `RUNMESH_STREAM_POLL_INTERVAL` can be relaxed. That is a change to one
knob, not to this decision.

The other reopening condition is a non-browser `EventSource` consumer, which
would make the stream-ticket alternative below worth revisiting.

## Alternatives considered

- **WebSocket via `gorilla/websocket` or `coder/websocket`, as the plan
  document says.** It loses on four independent counts, any one of which would
  be sufficient. It buys a client-to-server channel nothing uses. The upgrade
  needs `http.Hijacker`, and a hijacked connection throws away the entire
  middleware chain for its lifetime — `RequestID`, `Logger`'s status and byte
  accounting, `Recover`, and the drain signal all stop applying, so the drain
  problem gets *worse* rather than better. It is a fifth direct dependency
  against ADR 0007 and against `internal/httpapi`'s own package doc, in a module
  where `internal/gemini` hand-writes an HTTP client rather than take an SDK.
  And it ships no resume protocol, so the `id`/`Last-Event-ID` mechanism that
  maps one-to-one onto the existing gap-free per-job cursor would have to be
  hand-rolled, and then a reconnect loop hand-rolled to drive it. Note that the
  `statusRecorder` fix is *not* a tiebreaker: both transports need it.
- **Poll `GET /api/v1/jobs/{id}/events` on a 1s timer** (and `GET
  /api/v1/jobs/{id}` with its `ETag`). Not a joke, and it deserves to be said
  out loud: already implemented, already correct across replicas, already cheap
  — `getJob` sets an `ETag` from the job version explicitly so a dashboard can
  poll — and genuinely close on worst-case latency. It is rejected on
  **fidelity**, not latency: a 1s poll collapses a burst of transitions that
  happened 40 ms apart into one arrival, and the `STEP_SCHEDULED`-to-
  `STEP_STARTED` gap is exactly what a waterfall exists to show. It stays the
  documented fallback, which is why the `event` frame's payload is
  byte-identical to an element of that endpoint's `events` array.
- **`EventSource` plus a token in the query string.** Disqualified by this
  repository's own tests. `Logger` writes `slog.String("path", r.URL.Path)`, so
  `?token=` puts a live credential in every log line *by construction*, directly
  against `TestLogLineCarriesRouteAndKeyID`, which asserts the raw key never
  appears in one.
- **`EventSource` plus a session cookie.** `bearerToken` reads exactly one
  header and nothing else. A cookie is a second credential channel bolted onto a
  deliberately narrow scheme, and it pulls CSRF in behind it for an API that has
  no CSRF surface today.
- **`EventSource` plus a short-lived single-use stream ticket minted by an
  authenticated POST.** The only honourable way to make `EventSource` work, and
  the documented fallback if a non-browser `EventSource` consumer appears. It
  introduces mutable server-side token state into an otherwise stateless
  sha256-digest scheme, and that state has to be shared across replicas to be
  useful — a distributed cache, to support a browser API that `fetch` +
  `ReadableStream` makes unnecessary.
- **A store-wide `GET /api/v1/events` firehose keyed on `GlobalSeq`.**
  `internal/pgstore/events.go` states that `global_seq` allocation order is not
  commit order, so a live tail on that cursor can *permanently* miss an event
  whose lower sequence committed after a higher one was read. Making it safe
  needs an overlap window or a commit-ordered column — a store change, not a
  handler change. The per-job `Seq` is gap-free; scope the stream to a job.
- **Forward the store's `Subscribe` channel straight to the client, or serve the
  stream purely from the in-process bus.** Both stores' `Subscribe` is
  process-local, so a two-replica deployment would show each dashboard a
  silently incomplete subset. Shipping a stream that renders a
  plausible-but-wrong timeline is worse than polling.
- **Set `RUNMESH_WRITE_TIMEOUT` to 0 globally.** Removes slow-client protection
  from every other route to fix one. The per-connection escape is scoped to the
  stream and leaves the rest protected.
- **Disconnect, or coalesce, on a slow consumer.** Disconnecting just makes the
  client reconnect into the same backpressure. Coalescing is category-wrong for
  an append-only sequence: merging `STEP_STARTED` and `STEP_FINISHED` into "the
  latest one" destroys the waterfall the payload exists to draw. Because `Seq`
  is gap-free, a drop is detectable and repairable with one read.
- **Put the broker goroutine inside `internal/engine`.** The engine's package
  doc fixes the goroutine census at exactly 2 + Workers, plus one transient per
  in-flight step. A fan-out goroutine there contradicts a stated invariant that
  nothing would mechanically catch. It goes in `internal/eventbus`, constructed
  in `run()` like every other dependency.
- **Add `Subscribe` and `TailEvents` to `httpapi.Store`.** Unnecessary. The
  handler needs only `Job` and `JobEvents`, which that interface already has.
  Live push is a different concern with a different failure mode — lossy by
  design, process-local, optional — and folding it into the persistence
  interface would make every store implementation and every test double
  responsible for it. It arrives instead as a separate narrow `Streamer`
  interface declared next to `Runtime`, `Policy` and `Planner`, per the
  consumer-declared-interface convention.
