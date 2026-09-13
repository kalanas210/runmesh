# RunMesh console

The operator dashboard for the RunMesh runtime. Next.js 16 App Router, React 19,
Tailwind v4 configured CSS-first, TanStack Query for the data layer.

It is a token-for-token sibling of the ApexTick frontend — same warm near-black
canvas, same bone ink, same alpha-over-bone hairlines, same three type families,
same `cubic-bezier(0.16, 1, 0.3, 1)`, same grain — with one accent of its own and
an eight-value semantic state palette.

## Running it

```
cp .env.example .env.local         # then fill in RUNMESH_API_KEY
npm ci
npm run dev
```

`next dev` defaults to port 3000, which collides with Grafana's default. Check
before assuming it is free:

```
netstat -ano | findstr :3000
```

## The two environment variables

`RUNMESH_API_URL` and `RUNMESH_API_KEY` are read in Node by the route handler at
`app/api/rm/[...path]/route.ts` and by nothing else. Neither is prefixed
`NEXT_PUBLIC_`, so neither is inlined into the client bundle.

**They must not be added to the repository's top-level `.env.example`.** That
file asserts a one-to-one correspondence with `internal/config`'s loader — a
correspondence maintained by discipline rather than by a test — and
`internal/config` reads neither of these. They are documented in `web/.env.example`
instead, which is a separate file for a separate process.

## Why every request goes through `/api/rm`

The browser never talks to the Go API. Four problems collapse into one answer:

- **CORS.** There is no CORS middleware in the Go tree and adding one is not
  cheap. `Auth` runs outside the mux, and a preflight `OPTIONS` carries no
  `Authorization` header by spec, so it would be 401'd before routing — the CORS
  layer could not be a route, it would have to be a seventh middleware wrapping
  the other six. It would also need `Access-Control-Expose-Headers` for
  `ETag`, `X-Request-ID`, `Location`, `Retry-After` and `Idempotency-Replayed`,
  all five of which the API sets and none of which a cross-origin browser can
  otherwise read.
- **The key.** Sending a bearer from the browser means shipping the bearer to
  the browser.
- **The ETag poll.** `GET /jobs/{id}` sets `ETag: <version>` specifically so a
  dashboard can poll cheaply. That only works if `If-None-Match` goes up and
  `ETag` comes back down, which is exactly what the proxy forwards.
- **The confused deputy.** Holding the key and attaching it to whatever
  arrives makes this handler one by construction, so it has to establish that
  the caller is this app's own page before it acts. `crossSiteRefusal`
  (`lib/same-origin.ts`) refuses any state-changing request whose
  `Sec-Fetch-Site` is not `same-origin`/`none`, or whose `Origin` host is not
  the host the request was addressed to. A CSRF token would be the wrong
  instrument: this app has no session of its own to bind one to, so a token
  would be minted on request for anybody who can load the page.

The proxy costs one file and needs zero Go changes.

## Live updates

`lib/stream.ts` is a `fetch` + `ReadableStream` SSE reader, not `EventSource`.
`EventSource` cannot set a request header and the Go API's only credential path
is `Authorization: Bearer`; a query-parameter token would be printed into every
log line by the Logger middleware, against a test that exists to assert the key
never appears in one. `fetch` also buys manual control of the reconnect, which
`EventSource` does not give — it resumes on `Last-Event-ID` under a fixed
policy, and this endpoint resumes on `?after=<seq>` under a protocol that has a
distinct answer for "you have a hole in your history".

**The stream endpoint exists.** `GET /api/v1/jobs/{id}/stream`, in
`internal/httpapi/stream.go`, registered under `ScopeJobsRead`. Every claim in
`lib/stream.ts` has been checked against that handler rather than against the
document that once predicted it:

- frames are `id`/`event`/`data` triples and the names are exactly `snapshot`,
  `event`, `resync`, `end`, `bye` and `error` (`sseWriter.frame`);
- `id:` is the per-job `Seq` and is set only on `snapshot` and `event`, so a
  terminal frame can never move a client's cursor;
- resume is `?after=<seq>`, exclusive, with `Last-Event-ID` honoured only when
  the query parameter is absent (`streamCursor`);
- the heartbeat is a bare `: ping` comment, which the frame parser drops.

The reader stays deliberately tolerant where the handler is free to grow: an
unrecognised event name is surfaced rather than swallowed, because a frame this
build predates should make its own rollout visible, and a `snapshot` payload is
accepted as a bare array as well as the real `streamSnapshot` envelope.

### `live` means receiving, not connected

Two rules, and they exist because `eventsPollMs("live", false)` returns `false`
and so switches the fallback poll **off**:

- `status("open")` is not reported until a frame has actually been read off the
  wire. `response.ok` says a connection was established and a status line came
  back; it says nothing about whether anything will ever be delivered.
- A connection that delivers nothing at all — not one frame, not one heartbeat
  — for 40s is aborted, and the reconnect loop resumes it from the cursor. That
  is 2.7x the 15s `RUNMESH_STREAM_HEARTBEAT` default, so a single missed tick is
  tolerated and a dead socket is not. A deployment that widens the server's
  heartbeat must widen `idleTimeoutMs` with it.

Without either, an established-then-silent socket — a dropped NAT mapping, a
balancer that stopped forwarding without a FIN, a laptop resumed onto another
network — read as a healthy stream: the poll stayed off, the waterfall froze,
and the chip said LIVE with confidence. That is precisely the failure the
`Freshness` chip exists to make impossible, so the reader has to be able to
notice it.

The poll never goes away regardless; it slows from 1s to 5s. The stream carries
EVENTS, and the job row carries three things the events do not — `blocked_by`,
which is derived per request and stored nowhere, `result`, which is the tool's
output, and the rolled-up job state.

## Layout

```
app/
  globals.css           every token, the layers, the keyframes, the skip link
  layout.tsx            fonts, grain, skip link, providers, shell
  providers.tsx         the QueryClient
  api/rm/[...path]/     the API proxy, including the SSE pass-through
  design/               the primitives gallery (not linked from the nav)
components/
  ui/                   the primitives
  shell/                app shell and page header
lib/
  cn.ts                 clsx + tailwind-merge
  api.ts                the typed client and the error readers
  types.ts              the wire types, transcribed from the Go DTOs
  state.ts              the state vocabulary and its three carriers
  same-origin.ts        the proxy's origin guard, as a pure function
  stream.ts             the SSE reader, its frame parser and its reconnect
  feed.ts               the event merge, the transport chip, the poll intervals
  waterfall.ts          the event stream folded into per-attempt lanes
  format.ts             durations and instants, all pinned to UTC
```

## Two rules that are easy to break

**Tailwind v4 fails silently.** A token added to `:root` but omitted from the
`@theme inline` block compiles to nothing — no build error, no console warning,
just an unstyled element. Every new colour needs both entries. The gallery at
`/design` paints every swatch with a real utility written as a literal class
string, which is the cheapest way to notice.

**Colour is never the only carrier.** Every pill also says the word, carries a
glyph and wears its own stroke treatment; every waterfall segment also has a
texture. QUEUED and FAILED sit at nearly identical relative luminance and are
indistinguishable under full achromatopsia — dotted-hollow against solid-capped
is the only thing that separates them for that reader. Do not uniform the pill
strokes.
