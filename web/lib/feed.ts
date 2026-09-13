import type { RunMeshEvent } from "./types";

/**
 * The event feed's pure half: how two sources of the same events are merged,
 * and what the screen is allowed to claim about how it is being fed.
 *
 * ---------------------------------------------------------------------------
 * WHY MERGING NEEDS A FUNCTION AT ALL, rather than an array push.
 *
 * The job detail screen has two producers for one list. The SSE reader delivers
 * a `snapshot` page and then live frames; the polling fallback delivers pages
 * from `?after=`. They overlap on purpose — the stream's snapshot is a catch-up
 * page, a reconnect replays from the cursor, and a poll that raced a frame
 * fetches the same event twice. Appending would therefore produce duplicate
 * seqs, and a waterfall reducer that is handed the same STEP_SCHEDULED twice
 * opens the same attempt twice and draws a lane with phantom retries on it.
 *
 * So the merge is keyed on `seq`, which is per-job, 1-based and gap-free — the
 * only total order the runtime guarantees. `global_seq` is explicitly NOT used:
 * under PostgreSQL its allocation order is not its commit order, which the
 * migration documents as a caveat, so a list ordered by it can interleave two
 * jobs' events correctly and still put one job's own events out of order.
 * ---------------------------------------------------------------------------
 *
 * There is no React here and no clock. The transport state machine below is a
 * pure reducer for the same reason `lib/waterfall.ts` is pure: "did the console
 * correctly stop claiming to be live" is the assertion worth making
 * mechanically, and it cannot be made about a hook.
 */

/**
 * Merge an incoming page into the accumulated list, deduplicating on `seq`.
 *
 * Later wins on a collision. That is not arbitrary: the only way the same seq
 * arrives with a different body is a re-read of the same row, so the fresher
 * copy is the one to keep, and choosing "first wins" would mean a resync that
 * re-fetched a corrected history could never correct anything.
 *
 * Returns `current` ITSELF when nothing changed. React Query and useMemo both
 * compare by reference, and a merge that allocates a new array on every empty
 * poll re-runs `buildLanes` over the whole history once a second for a job
 * where nothing is happening.
 */
export function mergeEvents(
  current: readonly RunMeshEvent[],
  incoming: readonly RunMeshEvent[],
): RunMeshEvent[] {
  if (incoming.length === 0) return current as RunMeshEvent[];

  const bySeq = new Map<number, RunMeshEvent>();
  for (const event of current) bySeq.set(event.seq, event);

  let changed = false;
  for (const event of incoming) {
    const existing = bySeq.get(event.seq);
    // New, or held with a different body. Testing the BODY and not merely the
    // seq is what makes "later wins" true: a resync exists precisely so a
    // client can be handed a corrected history, and a merge that kept the copy
    // it already had could never apply one.
    if (existing === undefined || !sameEvent(existing, event)) changed = true;
    bySeq.set(event.seq, event);
  }
  if (!changed) {
    // Every incoming event was already held, byte for byte. This is the common
    // shape of a reconnect — the stream's snapshot is a catch-up page that
    // overlaps what the poller already fetched — and returning the same array
    // means the reducer does not re-run over the whole history because the
    // transport wobbled.
    return current as RunMeshEvent[];
  }

  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

/**
 * Whether two copies of one `seq` say the same thing.
 *
 * Only ever called on a collision, which is rare: `?after=` is exclusive, so a
 * poll returns only events the client does not have, and the overlap arrives
 * once per stream connection rather than once per second. That is what makes
 * the JSON comparison of `error` and `attrs` affordable — they are untyped
 * (`map[string]any` on the wire) and there is no cheaper structural check that
 * is also correct.
 *
 * The scalar comparison runs first and short-circuits, so the expensive half is
 * reached only for two events that agree on every field that has a type.
 */
function sameEvent(a: RunMeshEvent, b: RunMeshEvent): boolean {
  if (a === b) return true;
  if (
    a.seq !== b.seq ||
    a.global_seq !== b.global_seq ||
    a.job_id !== b.job_id ||
    a.step_id !== b.step_id ||
    a.attempt !== b.attempt ||
    a.type !== b.type ||
    a.at !== b.at ||
    a.state !== b.state ||
    a.duration_ms !== b.duration_ms
  ) {
    return false;
  }
  return stable(a.error) === stable(b.error) && stable(a.attrs) === stable(b.attrs);
}

/** An absent member and one serialising to "undefined" must not compare equal
 *  to each other by accident, hence the explicit empty string. */
function stable(value: unknown): string {
  return value === undefined || value === null ? "" : JSON.stringify(value);
}

/**
 * The resume cursor: the highest seq held, or 0 for an empty list.
 *
 * `?after=` is exclusive, so handing back the highest seq asks for everything
 * strictly after it. Deriving this from the list rather than tracking it in a
 * separate piece of state is deliberate — two numbers that must agree are two
 * numbers that eventually will not, and the list is the one that decides what
 * the chart draws.
 */
export function cursorOf(events: readonly RunMeshEvent[]): number {
  let max = 0;
  for (const event of events) if (event.seq > max) max = event.seq;
  return max;
}

/* -------------------------------------------------------------------------- */
/* What the accumulated list knows about its own completeness.                */
/* -------------------------------------------------------------------------- */

/**
 * The history warning's state: whether this client's timeline has a permanent
 * hole in it, and where.
 */
export interface HistoryPage {
  /**
   * LATCHED. True once any response has reported it, and cleared only when the
   * accumulated list it describes is thrown away.
   *
   * That distinction is the entire reason this type exists. `truncated` on the
   * wire is a fact about ONE request: the events endpoint compares that
   * request's `?after=` cursor against the ring's oldest surviving seq and says
   * whether anything between them is gone. The BANNER is a fact about the
   * accumulated list — "History truncated before seq N", drawn by
   * waterfall.tsx over a chart assembled from every page so far.
   *
   * Overwriting one with the other was a bug that erased itself. The cursor
   * advances with every poll, so the very next request asks for events that are
   * all still retained, answers `truncated: false`, and the warning vanishes —
   * on a job whose history is permanently, irrecoverably incomplete. The
   * reader is then shown a chart with attempts missing from the middle of it
   * and told nothing, which is strictly worse than the chart having refused to
   * draw: eventRing is a fixed-capacity FIFO (memstore/events.go:14) and what
   * it evicted is not coming back from anywhere.
   */
  truncated: boolean;
  /**
   * The boundary of the hole: the oldest seq that was still retained when
   * truncation was FIRST reported.
   *
   * Frozen at that moment rather than tracking the latest response, because
   * the ring keeps evicting and `oldest_seq` keeps rising while this client
   * holds the events that were evicted after it started reading. Taking the
   * newest value would move the banner's boundary to the right, over events
   * the reader can plainly see on the chart, and claim they are missing.
   */
  oldestSeq: number;
}

/** A list with nothing in it makes no claim about its own completeness. */
export const HISTORY_PAGE_FRESH: HistoryPage = { truncated: false, oldestSeq: 0 };

/**
 * Fold one response's completeness report into the accumulated one.
 *
 * Returns `current` ITSELF when nothing changed, for the same reason
 * `mergeEvents` does: this runs on every poll of an idle job, and a new object
 * each time is a re-render of the job detail screen — and therefore of the
 * waterfall — because something the server said was identical.
 */
export function latchHistoryPage(
  current: HistoryPage,
  incoming: { truncated?: boolean; oldestSeq?: number },
): HistoryPage {
  const truncated = current.truncated || incoming.truncated === true;
  // Once the boundary is known it is fixed. Before that, follow the newest
  // report, which is the only value there is.
  const oldestSeq = current.truncated
    ? current.oldestSeq
    : (incoming.oldestSeq ?? current.oldestSeq);

  if (truncated === current.truncated && oldestSeq === current.oldestSeq) return current;
  return { truncated, oldestSeq };
}

/* -------------------------------------------------------------------------- */
/* The transport state machine.                                               */
/* -------------------------------------------------------------------------- */

/**
 * How this screen is currently being fed, in the words it is allowed to use.
 *
 *   connecting  the stream is being opened and nothing has arrived yet
 *   live        the stream is open; events arrive as they happen
 *   polling     the stream is not available and the poller is carrying it
 *   final       the job is terminal and the stream said `end`; nothing follows
 *
 * There is deliberately no "error" member. A transport failure is not a state
 * the reader needs a word for — what they need to know is whether they are
 * still seeing the job, and the answer when the stream dies is "yes, slower",
 * which is `polling`. A screen that showed a red transport error and stopped
 * fetching would be reporting its own plumbing instead of the job.
 */
export type Transport = "connecting" | "live" | "polling" | "final";

export interface TransportState {
  mode: Transport;
  /**
   * Why the stream is not carrying this screen, in a sentence, or undefined
   * while it is.
   *
   * This is the whole point of the exercise. A dashboard that silently drops
   * from a live stream to a one-second poll is telling the reader nothing is
   * wrong; the events still arrive, just later, and the only symptom is that
   * the waterfall's right edge lags in a way nobody can attribute. Naming the
   * reason costs one line of chrome and turns an invisible degradation into a
   * fact — and on this API the most likely reason by far is a key without the
   * jobs.read scope, which is a configuration error the reader can fix.
   */
  reason?: string;
  /**
   * The stream has failed in a way that will not resolve by retrying, so the
   * reader is on the poller permanently rather than momentarily. Separated
   * from `reason` because the chrome says different things: a momentary drop
   * is "reconnecting", a permanent one is "polling — the stream is unavailable".
   */
  permanent: boolean;
}

export const TRANSPORT_CONNECTING: TransportState = { mode: "connecting", permanent: false };

/**
 * Statuses the stream reader reports, folded into the four words above.
 *
 * `reconnecting` becomes `polling` rather than staying "live but wobbling",
 * because from the reader's point of view those are the same situation — the
 * stream is not currently delivering and the poll is — and a chip that said
 * "live" while the socket was down would be exactly the lie this file exists
 * to prevent.
 */
export function transportFromStream(
  status: "connecting" | "open" | "reconnecting" | "closed",
  previous: TransportState,
): TransportState {
  switch (status) {
    case "connecting":
      // Only from a standing start. A reconnect that goes through `connecting`
      // on its way back must not flip the chip to a hopeful "connecting" after
      // it has already told the reader it degraded.
      return previous.mode === "polling" ? previous : TRANSPORT_CONNECTING;
    case "open":
      return { mode: "live", permanent: false };
    case "reconnecting":
      return {
        mode: "polling",
        reason: previous.reason ?? "the event stream dropped and is being reopened",
        permanent: previous.permanent,
      };
    case "closed":
      // `closed` after an `end` frame is the finished case and the caller marks
      // it; `closed` any other way means the reader gave up, which on this
      // implementation only happens for a status in FATAL_STATUSES.
      return previous.mode === "final"
        ? previous
        : {
            mode: "polling",
            reason: previous.reason ?? "the event stream closed",
            permanent: true,
          };
  }
}

/**
 * A stream error, turned into the sentence the chip prints.
 *
 * The HTTP status is worth naming for exactly three values. A 404 means this
 * build of the API predates the stream route, which is a deployment fact and
 * not a fault. A 403 means the key is missing jobs.read, which is the single
 * most likely real cause and is fixable by whoever reads it. A 501 means the
 * route exists and refuses. Everything else is transport noise and gets the
 * generic sentence, because inventing prose for a 502 from an unknown proxy
 * would be guessing on the reader's behalf.
 */
export function streamFailureReason(error: unknown): { reason: string; permanent: boolean } {
  const status =
    error && typeof error === "object" && "status" in error
      ? Number((error as { status: unknown }).status)
      : undefined;

  if (status === 404 || status === 405) {
    return {
      reason: "this API build has no live stream route, so the console is polling",
      permanent: true,
    };
  }
  if (status === 401 || status === 403) {
    return {
      reason: "the console's API key may not read this job's stream, so it is polling",
      permanent: true,
    };
  }
  if (status === 501) {
    return { reason: "the live stream is not enabled on this deployment", permanent: true };
  }
  return { reason: "the live stream is not connected, so the console is polling", permanent: false };
}

/**
 * The poll interval for the job snapshot, in milliseconds.
 *
 * `false` stops React Query's timer entirely, which is the correct answer for a
 * terminal job: its row cannot change again, the ETag would answer 304 for ever,
 * and a console left open on a finished job should cost the runtime nothing.
 *
 * The live case is 5000 and not 1000, and that is the reason the stream is
 * worth having at all. The stream carries EVENTS; it does not carry the job
 * row, and the job row holds three things events do not — `blocked_by`, which
 * is derived per request and stored nowhere, `result`, which is the tool's
 * output, and the rolled-up job state. So the poll never goes away; it only
 * slows down, and saying that plainly is better than pretending a live stream
 * removed it.
 */
export function jobPollMs(transport: Transport, terminal: boolean): number | false {
  if (terminal) return false;
  return transport === "live" ? 5000 : 1000;
}

/**
 * The poll interval for the events page.
 *
 * Zero polling while the stream is live: the stream is the same data arriving
 * sooner, and running both would double the load on the endpoint the console is
 * meant to be gentle with. The moment the transport degrades this becomes the
 * only source, which is why the fallback is a real fetch loop rather than a
 * retry of the stream.
 */
export function eventsPollMs(transport: Transport, terminal: boolean): number | false {
  if (terminal) return false;
  return transport === "live" ? false : 1000;
}
