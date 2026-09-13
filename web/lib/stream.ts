import { RM_BASE } from "./api";
import type { RunMeshEvent } from "./types";

/**
 * The live event reader: fetch + ReadableStream, not EventSource.
 *
 * EventSource cannot set a request header — its constructor takes only
 * `{ withCredentials }` — and the Go API's ONLY accepted credential path is
 * `Authorization: Bearer`. There is no cookie path and no query-parameter
 * path, and a query-parameter token would be actively wrong here: the Logger
 * middleware writes `slog.String("path", r.URL.Path)` on every request, so a
 * `?token=` would be printed into every log line, against a test that exists
 * specifically to assert the key never appears in one.
 *
 * fetch solves that without touching the auth scheme at all. It costs the
 * frame parser below, and it buys back manual control of the reconnect, which
 * EventSource does not give: EventSource resumes on Last-Event-ID with a fixed
 * policy, and this endpoint resumes on `?after=<seq>` with a protocol that has
 * a distinct "you have a hole in your history" answer.
 *
 * ---------------------------------------------------------------------------
 * A NOTE ON THE CONTRACT, and it matters.
 *
 * An earlier revision of this file said the endpoint did not exist and that
 * everything below was written against a specification. It exists now —
 * internal/httpapi/stream.go, registered as `GET /api/v1/jobs/{id}/stream`
 * under ScopeJobsRead — and every claim here has been checked against that
 * handler rather than against the document that predicted it:
 *
 *   - frames are id/event/data triples and the names are exactly `snapshot`,
 *     `event`, `resync`, `end`, `bye` and `error` (sseWriter.frame);
 *   - `id:` is the per-job `Seq` and is set only on `snapshot` and `event`, so
 *     a terminal frame can never move a client's cursor;
 *   - resume is `?after=<seq>`, exclusive, with `Last-Event-ID` honoured only
 *     when the query parameter is absent (streamCursor);
 *   - the heartbeat is a bare `: ping` comment, which parseFrame drops — but
 *     which the idle deadline counts as progress before it is dropped, since
 *     it is the only thing a healthy stream over a quiet job ever sends.
 *
 * The tolerances that were guesses are now deliberate choices. An unrecognised
 * event name is still surfaced rather than dropped, because the handler is
 * free to grow a frame this build predates and silently swallowing it would
 * make that rollout invisible. A `snapshot` payload is still accepted as a
 * bare array as well as the real `streamSnapshot` envelope, because that
 * branch costs one line and is the difference between a console that degrades
 * and one that renders an empty chart against an older API.
 *
 * What is NOT tolerated any more is the error frame: it carries the same
 * `{"error":{code,message,request_id}}` envelope every other route uses
 * (stream.go's `fail` routes through the same classifyError), so it is parsed
 * rather than stringified — see the `error` arm of dispatch.
 * ---------------------------------------------------------------------------
 *
 * Resume is on `seq`, never on `global_seq`. Seq is per-job, 1-based and
 * gap-free; global_seq's allocation order is not its commit order under
 * PostgreSQL, which the migration documents, so a tail resumed on it can skip
 * an event that committed late.
 */

/** The event names the stream is specified to send. */
export type StreamFrameName = "snapshot" | "event" | "resync" | "end" | "bye" | "error";

export interface StreamFrame {
  /** The `id:` field. The per-job seq, when the frame carries one. */
  id?: string;
  /** The `event:` field. Unrecognised names arrive here verbatim. */
  name: string;
  /** The joined `data:` lines, unparsed. */
  data: string;
}

export type StreamStatus = "connecting" | "open" | "reconnecting" | "closed";

export interface JobStreamHandlers {
  /** The catch-up page the server sends on connect, before live frames. */
  onSnapshot?: (events: RunMeshEvent[], frame: StreamFrame) => void;
  /** One live event. */
  onEvent?: (event: RunMeshEvent, frame: StreamFrame) => void;
  /**
   * The server lost events for this subscriber and the client's history now
   * has a hole. Subscribe drops for a slow consumer rather than blocking, so
   * this is a real condition and not a theoretical one. The correct response
   * is to discard accumulated state and re-read the snapshot — NOT to carry on
   * appending, which would draw a plausible and wrong timeline.
   */
  onResync?: (frame: StreamFrame) => void;
  /** The job reached a terminal state. The stream is complete; no reconnect. */
  onEnd?: (frame: StreamFrame) => void;
  /** The server is draining. Not an error, and the client should come back. */
  onBye?: (frame: StreamFrame) => void;
  /** A frame the contract does not name. Surfaced rather than dropped. */
  onUnknown?: (frame: StreamFrame) => void;
  /** A transport or protocol failure. The reader may still reconnect after it. */
  onError?: (error: unknown) => void;
  onStatus?: (status: StreamStatus) => void;
}

export interface JobStreamOptions extends JobStreamHandlers {
  jobId: string;
  /** Resume cursor. 0 or undefined asks for the whole retained history. */
  after?: number;
  /** First reconnect delay in ms. Doubles, with full jitter, to maxDelayMs. */
  baseDelayMs?: number;
  maxDelayMs?: number;
  /**
   * How long a connection may deliver NOTHING before it is treated as dead.
   * See IDLE_TIMEOUT_MS. Override it when the deployment's
   * RUNMESH_STREAM_HEARTBEAT is not the default.
   */
  idleTimeoutMs?: number;
  /** Caller-owned abort. Closing the handle aborts too. */
  signal?: AbortSignal;
}

export interface JobStreamHandle {
  close: () => void;
  /** The highest `seq` seen. The cursor a caller would resume from itself. */
  lastSeq: () => number;
}

/**
 * Statuses that will not change on a retry. Reconnecting into a 403 forever is
 * a denial of service aimed at one's own API, and it hides the real problem —
 * a key without the jobs.read scope — behind a spinner that never resolves.
 */
const FATAL_STATUSES = new Set([400, 401, 403, 404, 405, 410, 501]);

/**
 * How long a connection may deliver nothing at all before this reader gives up
 * on it, in ms.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS IS NOT OPTIONAL, and what happened without it.
 *
 * A TCP connection can be established and then silent for ever. A NAT table
 * that dropped the mapping, a load balancer that stopped forwarding without
 * sending a FIN, a laptop that came back from sleep onto a different network,
 * a Go process SIGKILLed mid-stream — in all of them the socket stays OPEN
 * from this side and `reader.read()` simply never resolves. There is no error
 * to catch and no end-of-body to notice.
 *
 * That failure used to read as a healthy stream, and it was worse than a
 * visible outage. `status("open")` fired on `response.ok`, transportFromStream
 * mapped it to `{mode:"live"}`, eventsPollMs("live", false) returned FALSE and
 * so switched the fallback poll OFF — and the chip then said LIVE, with
 * confidence, over a waterfall whose right edge had stopped moving and a
 * Freshness component whose entire purpose is to make exactly this
 * unmistakable.
 *
 * The server already solved its half: `sseWriter.ping` writes a bare `: ping`
 * comment on a ticker so intermediaries do not reap a correct-but-quiet
 * connection (stream.go:512). That heartbeat is also, for free, the liveness
 * PROOF this side needs — a connection that has not produced even a ping is
 * not merely quiet, it is gone. So the deadline is measured against the
 * heartbeat and not against anything about the job: a job can legitimately
 * emit no events for an hour, and no conclusion about the transport follows
 * from that.
 *
 * 40 seconds is 2.7x the 15-second RUNMESH_STREAM_HEARTBEAT default
 * (config.go:302). Two-and-a-bit intervals rather than one, because a single
 * missed tick is a GC pause or a scheduler hiccup and reconnecting over it
 * would replace a working stream with a reconnect storm; and not ten, because
 * a reader who has to stare at a frozen chart for two minutes before the chip
 * admits anything has learned nothing the chip did not already fail to tell
 * them. A deployment that widens the server's heartbeat must widen this too,
 * hence `idleTimeoutMs` on the options.
 * ---------------------------------------------------------------------------
 */
const IDLE_TIMEOUT_MS = 40_000;

class StreamHttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "StreamHttpError";
    this.status = status;
  }
}

/**
 * The connection went quiet past the deadline.
 *
 * A distinct class, not a bare Error, because it is the one transport failure
 * whose cause is worth naming to the reader: everything else this reader
 * throws is an HTTP status or a fetch rejection, and "the stream stopped
 * sending" is the diagnosis an operator cannot reach any other way. It is
 * deliberately NOT a StreamHttpError, so it can never land in FATAL_STATUSES
 * and stop the reconnect — a wedged connection is the case reconnecting was
 * built for.
 */
class StreamIdleError extends Error {
  readonly idleMs: number;
  constructor(idleMs: number) {
    super(
      `the event stream sent nothing for ${Math.round(idleMs / 1000)}s, not even a heartbeat, so it is being reopened`,
    );
    this.name = "StreamIdleError";
    this.idleMs = idleMs;
  }
}

/**
 * A re-armable idle deadline, as a promise that only ever rejects.
 *
 * Racing `reader.read()` against this is the only shape that works, and the
 * two obvious alternatives do not:
 *
 *   - A timer that merely aborts the fetch relies on the abort propagating
 *     into a pending `read()`, which is true of a real fetch body and not of
 *     any other ReadableStream — so the reader would still be parked on a read
 *     that never settles, and the failure would be untestable besides.
 *   - Comparing `Date.now()` against a stored last-arrival can only run when
 *     something arrives, which is precisely what has stopped happening. There
 *     is no loop iteration left to check it in.
 *
 * `progress()` re-arms, so the deadline measures the gap between bytes rather
 * than the age of the connection. A stream delivering a frame every second is
 * never a millisecond closer to the deadline than a stream delivering one
 * every thirty.
 */
function openIdleDeadline(
  idleMs: number,
  onExpire: () => void,
): {
  progress: () => void;
  race: <T>(pending: Promise<T>) => Promise<T>;
  clear: () => void;
} {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let expire: (reason: unknown) => void = () => {};
  // Constructed once, raced many times. Promise.race attaches a handler on
  // every call, so the eventual rejection is always observed and never
  // surfaces as an unhandled rejection.
  const expired = new Promise<never>((_, reject) => {
    expire = reject;
  });

  const arm = () => {
    if (timer !== undefined) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = undefined;
      // The socket first, then the rejection. Tearing down the fetch is what
      // frees the subscriber on the Go side — a stream handler's only
      // cancellation signal is r.Context().Done() — so a reader that merely
      // walked away would leave the server holding a channel for a client
      // that is never coming back.
      onExpire();
      expire(new StreamIdleError(idleMs));
    }, idleMs);
  };
  arm();

  return {
    progress: arm,
    race: (pending) => Promise.race([pending, expired]),
    clear: () => {
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
    },
  };
}

/**
 * Parse one SSE frame's raw text into fields.
 *
 * Per the event-stream grammar: a line beginning with ":" is a comment (used
 * as a keep-alive and carrying no meaning), the first ":" separates field from
 * value, a single leading space in the value is stripped, and repeated `data:`
 * lines are joined with newlines rather than the last one winning.
 */
function parseFrame(raw: string): StreamFrame | null {
  let id: string | undefined;
  let name = "message";
  const data: string[] = [];
  let sawField = false;

  for (const line of raw.split(/\r\n|\r|\n/)) {
    if (line === "" || line.startsWith(":")) continue;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);

    if (field === "id") {
      id = value;
      sawField = true;
    } else if (field === "event") {
      name = value;
      sawField = true;
    } else if (field === "data") {
      data.push(value);
      sawField = true;
    }
    // `retry:` is ignored on purpose: this reader owns its own backoff, and a
    // server-suggested interval would silently override the jitter that keeps
    // a fleet of reconnecting tabs from arriving in lockstep.
  }

  if (!sawField) return null;
  return { id, name, data: data.join("\n") };
}

function jsonOrUndefined(data: string): unknown {
  if (!data) return undefined;
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return undefined;
  }
}

/** A snapshot is accepted as a bare array or as the {events:[…]} page shape. */
function eventsFrom(payload: unknown): RunMeshEvent[] {
  if (Array.isArray(payload)) return payload as RunMeshEvent[];
  if (payload && typeof payload === "object" && "events" in payload) {
    const events = (payload as { events?: unknown }).events;
    if (Array.isArray(events)) return events as RunMeshEvent[];
  }
  return [];
}

/** Full jitter: delay is uniform in [0, capped), not capped exactly. */
function backoffMs(attempt: number, base: number, max: number): number {
  const ceiling = Math.min(max, base * 2 ** attempt);
  return Math.random() * ceiling;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(timer);
      resolve();
    }
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Open the stream and keep it open. Returns immediately; everything arrives
 * through the handlers.
 */
export function openJobStream(options: JobStreamOptions): JobStreamHandle {
  const {
    jobId,
    after = 0,
    baseDelayMs = 500,
    maxDelayMs = 15_000,
    idleTimeoutMs = IDLE_TIMEOUT_MS,
    signal: external,
    onSnapshot,
    onEvent,
    onResync,
    onEnd,
    onBye,
    onUnknown,
    onError,
    onStatus,
  } = options;

  const controller = new AbortController();
  const signal = controller.signal;
  if (external) {
    if (external.aborted) controller.abort();
    else external.addEventListener("abort", () => controller.abort(), { once: true });
  }

  let cursor = after;
  let finished = false;
  let attempt = 0;

  const status = (next: StreamStatus) => onStatus?.(next);

  /** Returns true when the stream said `end` and must not be reopened. */
  function dispatch(frame: StreamFrame): boolean {
    // The id is the per-job seq and is the authoritative cursor, because it is
    // set on every frame the server intends to be resumable from — including
    // ones whose payload this client does not understand.
    if (frame.id) {
      const seq = Number(frame.id);
      if (Number.isFinite(seq) && seq > cursor) cursor = seq;
    }

    switch (frame.name as StreamFrameName) {
      case "snapshot": {
        const events = eventsFrom(jsonOrUndefined(frame.data));
        for (const event of events) if (event.seq > cursor) cursor = event.seq;
        onSnapshot?.(events, frame);
        return false;
      }
      case "event": {
        const payload = jsonOrUndefined(frame.data) as RunMeshEvent | undefined;
        if (payload) {
          if (payload.seq > cursor) cursor = payload.seq;
          onEvent?.(payload, frame);
        }
        return false;
      }
      case "resync":
        onResync?.(frame);
        return false;
      case "end":
        onEnd?.(frame);
        return true;
      case "bye":
        // A drain, not a failure. Come back promptly rather than backing off
        // as though the server were broken: the replacement replica is already
        // starting, and resetting the attempt count is what makes the first
        // retry fast.
        attempt = 0;
        onBye?.(frame);
        return false;
      case "error":
        onError?.(new Error(frame.data || "the stream reported an error"));
        return false;
      default:
        onUnknown?.(frame);
        return false;
    }
  }

  /**
   * Read one connection to completion. Returns true if the stream said `end`.
   *
   * Throws StreamIdleError when the connection stops delivering, which the
   * caller treats as a disconnect and resumes from the cursor.
   */
  async function pump(
    body: ReadableStream<Uint8Array>,
    options: { idleMs: number; onIdle: () => void; onFrame: () => void },
  ): Promise<boolean> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    const deadline = openIdleDeadline(options.idleMs, options.onIdle);
    let buffer = "";
    try {
      for (;;) {
        const { done, value } = await deadline.race(reader.read());
        if (done) break;

        // Progress is recorded HERE, on the arrival of bytes, and before the
        // parser is given a chance to have an opinion about them. The
        // heartbeat is a bare `: ping` comment and parseFrame drops comments
        // on the floor by design, so a deadline armed on frames rather than on
        // bytes would expire on a perfectly healthy idle job — which is the
        // single most common state of a stream this reader will ever hold.
        deadline.progress();
        buffer += decoder.decode(value, { stream: true });

        // Frames are separated by a blank line. Everything after the last
        // separator is a partial frame and stays in the buffer: a chunk
        // boundary falls wherever TCP puts it, never where the protocol wants
        // it, and parsing a half-frame is how a reader drops one event in a
        // thousand and never finds out.
        let split = buffer.search(/\r\n\r\n|\n\n|\r\r/);
        while (split !== -1) {
          const raw = buffer.slice(0, split);
          const match = /\r\n\r\n|\n\n|\r\r/.exec(buffer.slice(split));
          buffer = buffer.slice(split + (match ? match[0].length : 2));
          const frame = parseFrame(raw);
          if (frame) {
            // A frame was read off the wire, which is the first moment this
            // connection has proved it can deliver one.
            options.onFrame();
            if (dispatch(frame)) return true;
          }
          split = buffer.search(/\r\n\r\n|\n\n|\r\r/);
        }
      }
    } finally {
      deadline.clear();
      // releaseLock rejects a read that is still pending, which is exactly the
      // state the idle path leaves behind. That rejection is observed — the
      // Promise.race above is holding a handler on the same promise — so it
      // cannot surface as an unhandled rejection.
      reader.releaseLock();
    }
    return false;
  }

  async function run(): Promise<void> {
    while (!signal.aborted && !finished) {
      // One AbortController per connection, chained to the caller's. The idle
      // deadline has to be able to drop THIS socket without ending the
      // stream, and aborting `controller` would do the latter: `signal` is
      // what `close()` uses and what every loop condition below reads, so a
      // wedged connection would look identical to the reader having left.
      const connection = new AbortController();
      const chain = () => connection.abort();
      signal.addEventListener("abort", chain, { once: true });

      /**
       * "open" is reported on the first FRAME, not on the response headers.
       *
       * `response.ok` only says a connection was established and a status line
       * came back. It says nothing about whether anything will ever be
       * delivered over it, and the difference is not academic: `live` switches
       * the fallback poll off, so a status derived from the headers alone is
       * how a silent socket came to be rendered as a healthy stream. The
       * handler writes a `snapshot` frame immediately on connect (stream.go),
       * so in the healthy case this fires microseconds later and no reader can
       * tell the difference — it costs nothing and it makes "live" mean
       * "receiving" rather than "connected".
       *
       * The backoff counter resets here for the same reason. A connection that
       * accepts and then delivers nothing has not succeeded, and treating it
       * as a success meant every such attempt reset the delay to base and
       * hammered a broken endpoint at full speed.
       */
      let live = false;
      const noteLive = () => {
        if (live) return;
        live = true;
        status("open");
        attempt = 0;
      };

      try {
        status(attempt === 0 ? "connecting" : "reconnecting");
        const url = `${RM_BASE}/jobs/${encodeURIComponent(jobId)}/stream?after=${cursor}`;
        const response = await fetch(url, {
          headers: { Accept: "text/event-stream" },
          cache: "no-store",
          signal: connection.signal,
        });

        if (!response.ok) {
          throw new StreamHttpError(
            response.status,
            `the event stream answered ${response.status}`,
          );
        }
        if (!response.body) {
          throw new Error("the event stream returned no body");
        }

        finished = await pump(response.body, {
          idleMs: idleTimeoutMs,
          onIdle: () => connection.abort(),
          onFrame: noteLive,
        });
      } catch (error) {
        if (signal.aborted) break;
        if (error instanceof StreamHttpError && FATAL_STATUSES.has(error.status)) {
          onError?.(error);
          break;
        }
        onError?.(error);
      } finally {
        signal.removeEventListener("abort", chain);
        // Nothing more will be read off this connection whichever way the try
        // block left. Without the abort a reconnect would leave the previous
        // socket open, and every one of them costs the Go side a live
        // subscriber until its own write deadline collects it.
        connection.abort();
      }

      if (finished || signal.aborted) break;

      // A clean end-of-body without an `end` frame is a severed connection,
      // not a finished job — the server's own absolute write deadline does
      // exactly this on a schedule — so it is treated as a disconnect and
      // resumed from the cursor, which is precisely what the cursor is for.
      status("reconnecting");
      await sleep(backoffMs(attempt, baseDelayMs, maxDelayMs), signal);
      attempt += 1;
    }
    status("closed");
  }

  void run();

  return {
    close: () => controller.abort(),
    lastSeq: () => cursor,
  };
}
