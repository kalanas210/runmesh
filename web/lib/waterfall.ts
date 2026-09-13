/**
 * The waterfall reducer: an event array folded into per-attempt lanes.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS EXISTS AT ALL, and why the obvious implementation is a lie.
 *
 * The obvious implementation reads `scheduled_at`, `started_at` and `ended_at`
 * off `stepResponse` and draws one bar per step. That renders a plausible,
 * confident, wrong chart on exactly the jobs an operator opened the dashboard
 * to understand, because the step row only ever describes the CURRENT attempt:
 *
 *   - claimOneLocked (memstore/claim.go:99) sets ScheduledAt = now, nulls
 *     StartedAt and EndedAt, clears Error and increments Attempt on EVERY
 *     claim. A step that failed twice and succeeded on the third attempt has
 *     no trace of attempts 1 and 2 in its row.
 *   - Release (memstore/finish.go:207) and ExpireLeases (finish.go:296) null
 *     ScheduledAt and StartedAt outright. A step requeued by a rolling restart
 *     or a dead worker loses its previous span from the row entirely.
 *
 * The events remember all of it and nothing else does. So the reconstruction
 * groups the stream by (step_id, attempt) and pairs
 *
 *   STEP_SCHEDULED -> STEP_STARTED -> one of
 *     STEP_FINISHED | STEP_RETRY_SCHEDULED | STEP_LEASE_EXPIRED | STEP_RELEASED
 *
 * and uses the snapshot ONLY for structure — depends_on, blocked_by,
 * max_attempts, timeout_seconds, the step's order in the array — and for the
 * current state of a lane. Not for a single bar edge.
 * ---------------------------------------------------------------------------
 *
 * There is no clock in this module and no React in it. `now` is a parameter of
 * the geometry functions below, never a read, which is the same rule the Go
 * side applies to internal/runmesh: a reducer that reads a clock cannot be
 * tested against a fixed history, and this reducer is where the correctness of
 * the whole screen lives.
 *
 * Every branch below exists because of a specific way the runtime can produce
 * a shape the naive version renders as NaN, as a zero-width invisible bar, or
 * as a silently discarded attempt. They are called out at each site.
 */

import { timeoutToMs } from "./format";
import { TERMINAL, type StepState } from "./state";
import type { ErrorInfo, JobResponse, RunMeshEvent, StepResponse } from "./types";

/**
 * One execution of one step.
 *
 * `n` is the domain's `Attempt` — monotonic, incremented on every claim, and
 * the thing that names an execution (it becomes the AttemptID the worker
 * carries). It is NOT the retry budget: `failures` is, and a step can
 * legitimately show n=4 with failures=1 because a release or a lease expiry
 * claims again without spending budget.
 */
export interface Attempt {
  stepId: string;
  n: number;
  /** The per-job seq of the event that OPENED this attempt. The stable sort
   *  key: two attempts can share an `n` (see the fork rule below) and only the
   *  sequence number orders them without ambiguity. */
  seq: number;

  /** Epoch milliseconds. Absent means the event that would have set it is not
   *  in the history — either it has not happened yet or it was evicted. */
  scheduledAt?: number;
  startedAt?: number;
  endedAt?: number;

  /** The state this attempt ENDED in, or the state it is currently in while
   *  open. Never UNKNOWN: the wire cannot carry it. */
  state: StepState;
  error?: ErrorInfo;

  /**
   * As REPORTED by the closing event, not recomputed from the timestamps.
   * The runtime measures `ended - Outcome.StartedAt`, which is the worker's
   * own view of tool execution time; recomputing it here from two RFC3339
   * strings would disagree with the number the API prints elsewhere on the
   * same screen for no benefit. Absent when the event omitted it — duration_ms
   * is omitempty and VANISHES at zero, which is a real value for a 0ms step.
   */
  durationMs?: number;

  /** STEP_RETRY_SCHEDULED only: attrs.next_attempt_at. The backoff IS this
   *  instant — there is no timer, the step is simply not claimable until the
   *  clock passes it. */
  backoffUntil?: number;
  /** attrs.failures from a retry or a lease expiry: the budget spent so far. */
  failures?: number;
  /** STEP_LEASE_EXPIRED only: attrs.owner, the worker that stopped answering. */
  expiredOwner?: string;
  /** STEP_RELEASED only: attrs.reason, e.g. the drain reason. */
  releasedReason?: string;

  /** No closing event has arrived. The right edge of this attempt is `now`,
   *  not `ended_at`, and it moves. */
  open: boolean;
  /**
   * Neither STEP_SCHEDULED nor STEP_STARTED was ever seen for this attempt,
   * yet it is closed. That is not corruption: jobstate.Reconcile emits a bare
   * STEP_FINISHED for a step cancelled before it started (code `cancelled`)
   * and for a doomed dependent (code `dependency_failed`), with ended_at set,
   * no duration, and no prior scheduling event. Such an attempt must render as
   * a terminal TICK, not as a bar of zero width that cannot be hovered.
   */
  neverRan: boolean;
  /**
   * This attempt spans zero measurable wall-clock time, so any renderer MUST
   * clamp it to a minimum width or it disappears from the chart entirely.
   *
   * Two real sources, not a theoretical one: a tool that returns in under the
   * timestamp resolution, and a fail-fast cancel or a doomed-dependent sweep,
   * which stamps EVERY affected step with the identical `now` (jobstate.go:174)
   * and so lands a whole column of them on one instant.
   *
   * Only ever set on a closed attempt. An open attempt's extent is whatever
   * `now` makes it and is not the reducer's to judge.
   */
  clamped: boolean;
}

/** One row of the chart: a step, its structure from the snapshot, and every
 *  attempt the surviving event history remembers. */
export interface Lane {
  stepId: string;
  tool: string;
  /** The step's position in the API's `steps` array, which IS plan ordinal.
   *  `ordinal` is not on the wire, so this order can never be re-derived after
   *  a client-side sort — hence it is captured here and preserved. */
  index: number;
  /** Topological depth: 0 for a step with no dependencies, otherwise one more
   *  than the deepest dependency. */
  depth: number;
  dependsOn: string[];
  /**
   * Derived per request by Job.BlockedBy and stored NOWHERE. Valid only for
   * the response that carried it; a lane object built from an older snapshot
   * shows blocking chips on steps that are already unblocked.
   */
  blockedBy: string[];
  /** The snapshot's current state for the step, which is the lane's state —
   *  distinct from the state of its last attempt (a lane can be QUEUED while
   *  its last attempt ended RETRYING). */
  state: StepState;
  /** The snapshot's current attempt number. */
  attempt: number;
  failures: number;
  maxAttempts: number;
  /** `timeout_seconds` is the one duration on this API in seconds, and a
   *  float. Converted once, here, so no call site has to remember. */
  timeoutMs: number;
  attempts: Attempt[];
}

/** A horizontal slice of one lane: what kind of interval it is, and when. */
export type SegmentKind = "queued" | "pending" | "run" | "backoff" | "tick";

export interface Segment {
  kind: SegmentKind;
  attempt: number;
  /** Epoch ms. `to === from` is legal and means a terminal tick. */
  from: number;
  to: number;
  /** Which palette entry paints it. */
  state: StepState;
  /** The right edge is `now` and will move on the next frame. */
  open: boolean;
  /**
   * The left edge is an INFERENCE, not data. Nothing in the runtime stores
   * when a step became eligible: NextAttemptAt is json:"-" and only surfaces
   * while a step is RETRYING. Any "queued for 1.2s" this chart shows is
   * derived from the job's creation and its dependencies' completion, and the
   * hover card has to say so.
   */
  inferred: boolean;
}

export interface DepthBand {
  depth: number;
  lanes: Lane[];
}

/**
 * What the chart knows about the completeness of its own input.
 *
 * `truncated: true` from the in-memory ring is not a cosmetic flag. It means
 * attempts are permanently gone — eventRing is a fixed-capacity FIFO
 * (memstore/events.go:14) and the default 512 entries evict on a long or
 * heavily retried job — and a chart that renders the survivors as though they
 * were the whole story is worse than no chart.
 */
export interface WaterfallHistory {
  truncated: boolean;
  /** The oldest per-job seq still retained. */
  oldestSeq: number;
  /** The earliest event instant still in hand, epoch ms. */
  earliestAt?: number;
  /**
   * JOB_CREATED is seq 1 and is always the first event a job ever emits. Its
   * absence from a history that claims not to be truncated means the caller
   * paged with `?after=` and never asked for the beginning — a different bug,
   * with the same consequence for the chart, so it is reported separately
   * rather than folded into `truncated`.
   */
  missingJobCreated: boolean;
}

/* -------------------------------------------------------------------------- */
/* Wire readers. Every one of these returns undefined rather than NaN.        */
/* -------------------------------------------------------------------------- */

/**
 * RFC3339 to epoch ms.
 *
 * The `Number.isNaN` guard is the single most load-bearing line in this file.
 * `new Date("").getTime()` is NaN, NaN propagates silently through every
 * arithmetic operation downstream, and the first visible symptom is an SVG
 * with `width="NaN"` that the browser drops without a console message. A
 * timestamp that cannot be parsed is ABSENT, and every consumer below already
 * has a branch for absent.
 */
function parseInstant(iso: string | null | undefined): number | undefined {
  if (!iso) return undefined;
  const ms = Date.parse(iso);
  return Number.isFinite(ms) ? ms : undefined;
}

/** `attrs` is map[string]any: untyped on the wire and different per event
 *  kind. There is no schema, so every read narrows first. */
function attrString(
  attrs: Record<string, unknown> | undefined,
  key: string,
): string | undefined {
  const value = attrs?.[key];
  return typeof value === "string" ? value : undefined;
}

function attrNumber(
  attrs: Record<string, unknown> | undefined,
  key: string,
): number | undefined {
  const value = attrs?.[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** attrs.next_attempt_at is a Go time.Time and marshals to an RFC3339 string.
 *  A number is accepted too, because a hand-built fixture is the likeliest
 *  second shape and rejecting it would fail loudly in a test and silently in
 *  production. */
function attrInstant(
  attrs: Record<string, unknown> | undefined,
  key: string,
): number | undefined {
  const value = attrs?.[key];
  if (typeof value === "string") return parseInstant(value);
  if (typeof value === "number" && Number.isFinite(value)) return value;
  return undefined;
}

/* -------------------------------------------------------------------------- */
/* Depth                                                                      */
/* -------------------------------------------------------------------------- */

/**
 * Topological depth, computed as the LONGEST path from a root rather than the
 * shortest.
 *
 * Shortest-path banding puts a step one hop below its nearest dependency even
 * when another dependency is five hops deep, which draws a band that cannot
 * start until long after the band below it has finished — the bands stop
 * meaning "these can run together", which is the only reason they are on the
 * screen.
 *
 * The `visiting` guard is not defensive theatre against a validated API:
 * Plan.Validate rejects cycles at submit time, so a cycle arriving here means
 * the payload is not what it claims to be, and a recursion that never returns
 * takes the browser tab with it. Returning 0 for the cyclic edge draws a
 * slightly wrong band; not returning at all draws nothing, ever.
 */
function computeDepths(steps: readonly StepResponse[]): Map<string, number> {
  const byId = new Map<string, StepResponse>();
  for (const step of steps) byId.set(step.id, step);

  const depths = new Map<string, number>();
  const visiting = new Set<string>();

  function depthOf(id: string): number {
    const memo = depths.get(id);
    if (memo !== undefined) return memo;
    if (visiting.has(id)) return 0;

    const step = byId.get(id);
    // An unknown dependency id cannot happen through a validated plan, but it
    // must not be allowed to poison the depth of the step that named it.
    if (!step) return 0;

    visiting.add(id);
    let depth = 0;
    for (const dependency of step.depends_on) {
      if (!byId.has(dependency)) continue;
      depth = Math.max(depth, depthOf(dependency) + 1);
    }
    visiting.delete(id);

    depths.set(id, depth);
    return depth;
  }

  for (const step of steps) depthOf(step.id);
  return depths;
}

/* -------------------------------------------------------------------------- */
/* The fold                                                                   */
/* -------------------------------------------------------------------------- */

interface Draft extends Attempt {
  /** Internal only: the key this draft was filed under, so a fork can claim a
   *  distinct one without colliding with a future real attempt. */
  key: string;
}

/**
 * Fold the event stream into lanes.
 *
 * Pure. No clock, no React, no network. `now` is deliberately NOT a parameter:
 * an open attempt is left open, with `endedAt` undefined and `open: true`, and
 * the moving right edge is applied later by `laneSegments`. That keeps the
 * expensive fold stable across the once-a-second tick that only moves one edge
 * of one bar, and it makes every assertion in the test suite a comparison
 * against a fixed history rather than against whatever the wall clock said.
 *
 * Events are sorted by `seq` before folding. `seq` is per-job, 1-based and
 * gap-free, so it is a total order over this job's history — which matters
 * because pages arrive concatenated, a live frame can land while a page is in
 * flight, and reading STEP_STARTED after STEP_FINISHED would reopen a closed
 * attempt.
 */
export function buildLanes(
  job: JobResponse,
  events: readonly RunMeshEvent[],
): Lane[] {
  const depths = computeDepths(job.steps);

  const lanes: Lane[] = job.steps.map((step, index) => ({
    stepId: step.id,
    tool: step.tool,
    index,
    depth: depths.get(step.id) ?? 0,
    dependsOn: step.depends_on,
    blockedBy: step.blocked_by,
    state: step.state,
    attempt: step.attempt,
    failures: step.failures,
    maxAttempts: step.max_attempts,
    timeoutMs: timeoutToMs(step.timeout_seconds),
    attempts: [],
  }));

  const laneById = new Map<string, Lane>();
  for (const lane of lanes) laneById.set(lane.stepId, lane);

  // Keyed by `${stepId}#${attempt}` plus, for a fork, a seq suffix.
  const drafts = new Map<string, Draft>();
  const order: Draft[] = [];

  for (const event of sortBySeq(events)) {
    // A caller that concatenates pages can, with one wrong query key in React
    // Query, splice another job's events into this array. Folding them would
    // produce attempts on lanes that never ran them, which looks like a
    // runtime bug and is not one.
    if (event.job_id && event.job_id !== job.id) continue;

    const stepId = event.step_id;
    if (!stepId) continue; // job-level event; see buildHistory
    // A step id the snapshot does not know cannot be drawn — there is no lane
    // for it and no structure to hang it on.
    if (!laneById.has(stepId)) continue;

    // `attempt` is omitempty and VANISHES at zero, which is every step event
    // before the first claim. Defaulting it is not a nicety: a strict read
    // would drop exactly the pre-start cancellations this chart has to show.
    const n = event.attempt ?? 0;
    const at = parseInstant(event.at);
    const baseKey = `${stepId}#${n}`;

    switch (event.type) {
      case "STEP_SCHEDULED": {
        const draft = open(baseKey, stepId, n, event.seq);
        draft.scheduledAt = at;
        draft.state = event.state ?? "SCHEDULED";
        break;
      }

      case "STEP_STARTED": {
        const draft = open(baseKey, stepId, n, event.seq);
        draft.startedAt = at;
        draft.state = event.state ?? "RUNNING";
        break;
      }

      case "STEP_FINISHED":
      case "STEP_RETRY_SCHEDULED":
      case "STEP_LEASE_EXPIRED":
      case "STEP_RELEASED": {
        const draft = closing(baseKey, stepId, n, event.seq);
        draft.endedAt = at;
        draft.open = false;
        // Every step event carries the step's state AFTER the transition
        // (jobstate.StepEvent), so this is the outcome and not a guess. The
        // fallbacks below are the documented value for each kind, used only if
        // a future producer starts omitting `state`.
        draft.state =
          event.state ??
          (event.type === "STEP_RETRY_SCHEDULED"
            ? "RETRYING"
            : event.type === "STEP_RELEASED"
              ? "QUEUED"
              : "FAILED");
        if (event.error) draft.error = event.error;
        if (event.duration_ms !== undefined) draft.durationMs = event.duration_ms;

        if (event.type === "STEP_RETRY_SCHEDULED") {
          draft.backoffUntil = attrInstant(event.attrs, "next_attempt_at");
          draft.failures = attrNumber(event.attrs, "failures");
        } else if (event.type === "STEP_LEASE_EXPIRED") {
          draft.expiredOwner = attrString(event.attrs, "owner");
          draft.failures = attrNumber(event.attrs, "failures");
        } else if (event.type === "STEP_RELEASED") {
          draft.releasedReason = attrString(event.attrs, "reason");
        }
        break;
      }

      default:
        // The four reserved kinds (POD_CREATED, POD_DELETED, TOOL_CALLED,
        // STEP_OUTPUT_CHUNK) have no producer anywhere in the repo, and the
        // job-level kinds have no step_id and were skipped above. Ignoring an
        // unrecognised type is correct: the vocabulary is allowed to grow, and
        // a chart that throws on a kind it has not been taught about is a
        // chart that breaks on a deploy.
        break;
    }
  }

  function open(key: string, stepId: string, n: number, seq: number): Draft {
    const existing = drafts.get(key);
    // A CLOSED attempt must never be reopened. Post-sort this cannot happen
    // through ordering, but it can through the fork rule below handing back a
    // key whose draft has already settled.
    if (existing && existing.open) return existing;
    if (existing && !existing.open) return fork(key, stepId, n, seq);
    return create(key, stepId, n, seq);
  }

  /**
   * Find the draft a closing event should settle — forking a new one when the
   * attempt it names is already closed.
   *
   * This is not a paranoid branch, it is the cancelled-job case. A step that
   * failed and went RETRYING has closed attempt n. If the job is then
   * cancelled, jobstate.Reconcile emits STEP_FINISHED with state CANCELLED for
   * that same step, carrying the SAME `attempt` number, because no claim
   * happened in between to increment it. Overwriting would destroy the record
   * of the failure and the backoff — the two facts a reader opened the chart
   * for — and replace them with a cancellation. Forking keeps both, and the
   * fork has no scheduling events of its own, so it renders as the terminal
   * tick it actually is.
   */
  function closing(key: string, stepId: string, n: number, seq: number): Draft {
    const existing = drafts.get(key);
    if (!existing) return create(key, stepId, n, seq);
    if (existing.open) return existing;
    return fork(key, stepId, n, seq);
  }

  function fork(key: string, stepId: string, n: number, seq: number): Draft {
    return create(`${key}@${seq}`, stepId, n, seq);
  }

  function create(key: string, stepId: string, n: number, seq: number): Draft {
    const draft: Draft = {
      key,
      stepId,
      n,
      seq,
      state: "QUEUED",
      open: true,
      neverRan: true,
      clamped: false,
    };
    drafts.set(key, draft);
    order.push(draft);
    return draft;
  }

  for (const draft of order) {
    const lane = laneById.get(draft.stepId);
    if (!lane) continue;
    lane.attempts.push(finalise(draft));
  }

  for (const lane of lanes) {
    // Sort by the opening seq, not by `n`. Two attempts can carry the same `n`
    // after a fork, and the sequence number is the only total order the
    // runtime guarantees.
    lane.attempts.sort((a, b) => a.seq - b.seq);
  }

  return lanes;
}

function finalise(draft: Draft): Attempt {
  const { key: _key, ...attempt } = draft;
  void _key;

  attempt.neverRan =
    attempt.scheduledAt === undefined && attempt.startedAt === undefined;

  const known = [attempt.scheduledAt, attempt.startedAt, attempt.endedAt].filter(
    (value): value is number => value !== undefined,
  );
  // Only a CLOSED attempt can be judged to have no extent. An open one is as
  // wide as `now` makes it, and `now` is not this function's business.
  attempt.clamped =
    !attempt.open && known.length > 0 && Math.max(...known) - Math.min(...known) === 0;

  return attempt;
}

/** `seq` is gap-free and 1-based per job, so it is a total order. The index
 *  tiebreaker keeps the sort stable for a caller that (wrongly) repeats one. */
function sortBySeq(events: readonly RunMeshEvent[]): RunMeshEvent[] {
  return events
    .map((event, index) => ({ event, index }))
    .sort((a, b) => a.event.seq - b.event.seq || a.index - b.index)
    .map((entry) => entry.event);
}

/* -------------------------------------------------------------------------- */
/* Structure derived from the lanes                                           */
/* -------------------------------------------------------------------------- */

/**
 * Group lanes into topological depth bands, preserving the API's step order
 * within each band.
 *
 * The bands are how this chart carries DAG shape without drawing a single
 * edge. Fan-out plans — N fetches, then an analyze, then a report — produce an
 * unreadable weave at even ten steps if every dependency is a line, and the
 * band layout says the same thing statically and costs no ink.
 */
export function depthBands(lanes: readonly Lane[]): DepthBand[] {
  const byDepth = new Map<number, Lane[]>();
  for (const lane of lanes) {
    const bucket = byDepth.get(lane.depth);
    if (bucket) bucket.push(lane);
    else byDepth.set(lane.depth, [lane]);
  }
  return [...byDepth.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([depth, bandLanes]) => ({
      depth,
      lanes: [...bandLanes].sort((a, b) => a.index - b.index),
    }));
}

export interface QueuedAnchor {
  at: number;
  /** Always true. It is a field rather than a constant so a caller reading the
   *  return value cannot forget, and so the hover card can print the word. */
  inferred: true;
}

/**
 * The left edge of a lane's first queued segment.
 *
 * There is NO per-step eligible-at timestamp anywhere in the runtime. Nothing
 * records when a step became claimable: NextAttemptAt is json:"-" and only
 * surfaces, as next_attempt_at, while a step is RETRYING. So the earliest
 * instant at which this step COULD have been claimed is inferred as the later
 * of the job's creation and the completion of its last dependency, and the
 * `inferred` flag travels with the number so no call site can present it as
 * data.
 *
 * A dependency whose completion is unknown — evicted from a truncated ring, or
 * simply not finished — contributes nothing rather than contributing a guess.
 */
export function queuedAnchor(
  lane: Lane,
  job: JobResponse,
  lanes: readonly Lane[],
): QueuedAnchor {
  const laneById = new Map(lanes.map((entry) => [entry.stepId, entry]));
  let at = parseInstant(job.created_at) ?? 0;

  for (const dependency of lane.dependsOn) {
    const dependencyLane = laneById.get(dependency);
    if (!dependencyLane) continue;
    const ended = lastEnd(dependencyLane);
    if (ended !== undefined && ended > at) at = ended;
  }
  return { at, inferred: true };
}

function lastEnd(lane: Lane): number | undefined {
  let latest: number | undefined;
  for (const attempt of lane.attempts) {
    if (attempt.endedAt !== undefined && (latest === undefined || attempt.endedAt > latest)) {
      latest = attempt.endedAt;
    }
  }
  return latest;
}

/* -------------------------------------------------------------------------- */
/* Geometry, in time. Pixels happen in lib/scale.ts.                          */
/* -------------------------------------------------------------------------- */

/**
 * A lane's attempts expanded into the intervals the chart draws.
 *
 * Three intervals per attempt, and the middle one is the reason this chart is
 * worth building:
 *
 *   queued   anchor (or the previous attempt's backoff) -> scheduled_at
 *   pending  scheduled_at -> started_at
 *   run      started_at -> ended_at (or `now`, while open)
 *
 * `pending` is the gap between a worker claiming the step and the tool
 * actually beginning. Under the in-process executor it is microseconds; under
 * the Kubernetes executor it is the pod-pending wait and is routinely seconds,
 * which is exactly the kind of time that vanishes from every summary. It must
 * never be merged into the running bar — `duration_ms` already excludes it, so
 * a chart that merges them disagrees with the number printed beside it.
 *
 * `now` enters here and only here.
 */
export function laneSegments(
  lane: Lane,
  options: { anchor: number; now: number },
): Segment[] {
  const { anchor, now } = options;
  const segments: Segment[] = [];
  let cursor = anchor;
  let first = true;

  for (const attempt of lane.attempts) {
    // A pre-start cancellation or a doomed dependent: ended_at and nothing
    // else. A zero-width bar here would be invisible and un-hoverable, and it
    // is precisely the case a reader is looking for on a failed job, so it
    // gets its own kind and the renderer draws it as a tick.
    if (attempt.neverRan) {
      if (attempt.endedAt !== undefined) {
        if (attempt.endedAt > cursor) {
          segments.push({
            kind: "queued",
            attempt: attempt.n,
            from: cursor,
            to: attempt.endedAt,
            state: "QUEUED",
            open: false,
            inferred: first,
          });
        }
        segments.push({
          kind: "tick",
          attempt: attempt.n,
          from: attempt.endedAt,
          to: attempt.endedAt,
          state: attempt.state,
          open: false,
          inferred: false,
        });
        cursor = Math.max(cursor, attempt.endedAt);
      }
      first = false;
      continue;
    }

    const claimed = attempt.scheduledAt ?? attempt.startedAt;
    if (claimed !== undefined && claimed > cursor) {
      segments.push({
        kind: "queued",
        attempt: attempt.n,
        from: cursor,
        to: claimed,
        state: "QUEUED",
        open: false,
        inferred: first,
      });
    }

    // The right edge of an unfinished pending span is `now`: the step has been
    // claimed and has not started, and that wait is happening as the reader
    // watches.
    if (attempt.scheduledAt !== undefined) {
      const pendingEnd = attempt.startedAt ?? attempt.endedAt ?? (attempt.open ? now : undefined);
      if (pendingEnd !== undefined && pendingEnd > attempt.scheduledAt) {
        segments.push({
          kind: "pending",
          attempt: attempt.n,
          from: attempt.scheduledAt,
          to: pendingEnd,
          state: "SCHEDULED",
          open: attempt.open && attempt.startedAt === undefined,
          inferred: false,
        });
      }
      cursor = Math.max(cursor, attempt.scheduledAt);
    }

    if (attempt.startedAt !== undefined) {
      const runEnd = attempt.endedAt ?? (attempt.open ? now : attempt.startedAt);
      segments.push({
        kind: "run",
        attempt: attempt.n,
        from: attempt.startedAt,
        // An open attempt whose start is in the future relative to `now` — a
        // clock skew between the worker and this browser — would otherwise
        // produce a negative width.
        to: Math.max(runEnd, attempt.startedAt),
        state: attempt.open ? "RUNNING" : attempt.state,
        open: attempt.open,
        inferred: false,
      });
      cursor = Math.max(cursor, attempt.endedAt ?? attempt.startedAt);
    } else if (attempt.endedAt !== undefined) {
      // Scheduled, never started, then closed: a release or a lease expiry
      // that caught the step in the gap. There is no run span at all, and the
      // pending span above already covers the interval.
      cursor = Math.max(cursor, attempt.endedAt);
    }

    // The backoff. Drawn as its own interval rather than left as dead space,
    // because "this step waited 40 minutes between attempts" is a fact about
    // the retry policy and reads as a chart bug when it is blank.
    if (attempt.backoffUntil !== undefined && attempt.endedAt !== undefined) {
      const until = Math.max(attempt.backoffUntil, attempt.endedAt);
      // `next_attempt_at` is a FUTURE instant, and this is the only interval
      // in this function that arrives from the wire as a prediction rather
      // than as a record. Drawn unclamped it put a bar to the RIGHT of now,
      // past the axis's own right edge and past every other in-progress
      // interval here — `run` and `pending` both stop at `now` — and it added
      // the unelapsed remainder to `backoffMs`, so nine seconds into a
      // forty-minute wait the hover card claimed forty minutes had been spent.
      //
      // So the DRAWN extent stops at `now` and the segment is marked open,
      // which is the same treatment the other two get and means the same
      // thing: this is still happening and the right edge moves. It also
      // earns the shimmer in attempt-bar.tsx, which is correct — a step inside
      // its backoff is as much "in progress" as one inside its tool call.
      //
      // `attempt.backoffUntil` is deliberately left whole. The future instant
      // is a fact and the hover card's "backoff until" row is where it
      // belongs; it is simply not a drawn extent.
      const drawnTo = Math.min(until, Math.max(now, attempt.endedAt));
      if (drawnTo > attempt.endedAt) {
        segments.push({
          kind: "backoff",
          attempt: attempt.n,
          from: attempt.endedAt,
          to: drawnTo,
          state: "RETRYING",
          open: until > now,
          inferred: false,
        });
      }
      // The CURSOR still advances to the true end rather than to the clamped
      // one. It is where the next attempt's queued lead-in begins, and a step
      // is not claimable until the clock passes `next_attempt_at`, so
      // anchoring that lead-in at `now` would invent queued time the retry
      // policy forbids — and would then draw it twice, once as the tail below
      // and once as the next attempt's lead-in.
      cursor = Math.max(cursor, until);
    }

    first = false;
  }

  // The tail: a lane that is still waiting. Its last attempt is closed (it
  // retried, was released, or lost its lease) and the next claim has not
  // happened, so the interval from there to now is real queued time that no
  // attempt owns. Terminal lanes get nothing — a finished step is not waiting.
  //
  // TWO GATES, not one, and the second is the reason this module exists.
  // `lane.state` comes from the SNAPSHOT, which is polled every 5000ms while
  // the stream is healthy (feed.ts, jobPollMs); the attempts come from the
  // EVENT STREAM, where STEP_FINISHED arrives the instant it is emitted. Those
  // two speeds are not a detail — gated on the snapshot alone, for up to five
  // seconds after a step succeeded the chart drew a queued bar growing to the
  // right off a step that had already finished, because the row still said
  // QUEUED or RUNNING. A phantom, on the one screen whose whole premise is
  // that the stream is the fresher of the two sources.
  //
  // So a lane whose last attempt is CLOSED in a terminal state gets no tail
  // whatever the snapshot says. Only STEP_FINISHED can produce that shape:
  // a retry closes an attempt as RETRYING, a release as QUEUED and a lease
  // expiry back into the queue, none of which are terminal and all of which
  // genuinely are still waiting for the next claim.
  const lastAttempt = lane.attempts[lane.attempts.length - 1];
  const settled =
    lastAttempt !== undefined && !lastAttempt.open && TERMINAL.has(lastAttempt.state);

  if (!TERMINAL.has(lane.state) && !settled && now > cursor) {
    const anyOpen = lane.attempts.some((attempt) => attempt.open);
    if (!anyOpen) {
      segments.push({
        kind: "queued",
        attempt: lane.attempt,
        from: cursor,
        to: now,
        state: lane.state === "RETRYING" ? "RETRYING" : "QUEUED",
        open: true,
        inferred: lane.attempts.length === 0,
      });
    }
  }

  return segments;
}

/**
 * The attempt a segment belongs to.
 *
 * Not a lookup by number, because two attempts can legitimately carry the SAME
 * number. A cancel that lands on a step already closed as RETRYING reuses the
 * attempt number — no claim happened in between to increment it — so the fold
 * keeps both and they share an `n`. Taking the first match would hang the
 * cancellation's tick on the failed attempt and print the failure's error
 * beside a bar that was never a failure.
 *
 * The tiebreak is the segment's own left edge: of the candidates that had
 * started by then, the latest one is the one this segment came out of.
 */
export function attemptAt(lane: Lane, segment: Segment): Attempt | undefined {
  const candidates = lane.attempts.filter((attempt) => attempt.n === segment.attempt);
  if (candidates.length <= 1) return candidates[0];

  let best = candidates[0];
  let bestAt = -Infinity;
  for (const attempt of candidates) {
    const opened = firstInstant(attempt);
    if (opened !== undefined && opened <= segment.from && opened >= bestAt) {
      best = attempt;
      bestAt = opened;
    }
  }
  return best;
}

function firstInstant(attempt: Attempt): number | undefined {
  const known = [attempt.scheduledAt, attempt.startedAt, attempt.endedAt].filter(
    (value): value is number => value !== undefined,
  );
  return known.length > 0 ? Math.min(...known) : undefined;
}

/** How long a lane spent in each kind of interval, and whether any of it was
 *  inferred rather than recorded. */
export interface SpanTotals {
  queuedMs: number;
  pendingMs: number;
  runMs: number;
  backoffMs: number;
  /**
   * At least one queued span's LEFT edge came from `queuedAnchor` rather than
   * from an event. It travels with the numbers because the three places that
   * print them — the hover card, the accessible table and the lane's spoken
   * label — must each say the word "inferred", and a flag on the totals is the
   * only way none of them can forget.
   */
  inferred: boolean;
}

export function spanTotals(segments: readonly Segment[]): SpanTotals {
  const totals: SpanTotals = {
    queuedMs: 0,
    pendingMs: 0,
    runMs: 0,
    backoffMs: 0,
    inferred: false,
  };
  for (const segment of segments) {
    const extent = Math.max(segment.to - segment.from, 0);
    switch (segment.kind) {
      case "queued":
        totals.queuedMs += extent;
        if (segment.inferred) totals.inferred = true;
        break;
      case "pending":
        totals.pendingMs += extent;
        break;
      case "run":
        totals.runMs += extent;
        break;
      case "backoff":
        totals.backoffMs += extent;
        break;
      case "tick":
        // An instant, not an interval. Adding zero would be harmless and
        // adding it to any bucket would be a lie about which kind of time it
        // was, so it belongs to none of them.
        break;
    }
  }
  return totals;
}

/**
 * A lane as one spoken sentence.
 *
 * This is the chart for a reader who has no chart: it is the `aria-label` on
 * the lane row, and a screen-reader user arrowing down the lanes hears this and
 * nothing else. So it carries every fact the bars carry — the state, how many
 * attempts there were, where the time went, and what went wrong — in the order
 * a sighted reader's eye takes them, and it NEVER refers to colour or position.
 * "The red bar" would be useless to exactly the person reading it.
 *
 * It lives here, in the pure module, rather than in the component, because a
 * sentence assembled inline in JSX is a sentence nothing can test, and this one
 * has branches: an inferred left edge, a lane that never ran at all, an error
 * that may or may not be present.
 */
export function describeLane(
  lane: Lane,
  segments: readonly Segment[],
  labelOf: (state: StepState) => string,
  durationOf: (ms: number) => string,
): string {
  const totals = spanTotals(segments);
  const parts: string[] = [
    `${lane.stepId}, tool ${lane.tool}, depth ${lane.depth}, ${labelOf(lane.state)}`,
  ];

  const recorded = lane.attempts.length;
  if (recorded === 0) {
    parts.push("no attempts recorded");
  } else if (lane.attempt > recorded) {
    // `attempts.length` and `lane.attempt` are different numbers on a
    // truncated history: the row remembers the runtime's count, this array
    // holds only what survived the ring. Announcing the row's count would
    // promise bars a reader will never find, and announcing only this array's
    // count would hide that some are missing. Saying both is the one honest
    // sentence.
    parts.push(
      `${recorded} ${recorded === 1 ? "attempt" : "attempts"} recorded of ${lane.attempt}`,
    );
  } else {
    parts.push(`${recorded} ${recorded === 1 ? "attempt" : "attempts"}`);
  }

  const spans: string[] = [];
  if (totals.queuedMs > 0) {
    spans.push(
      `queued ${durationOf(totals.queuedMs)}${totals.inferred ? " (inferred)" : ""}`,
    );
  }
  if (totals.pendingMs > 0) spans.push(`pending ${durationOf(totals.pendingMs)}`);
  if (totals.runMs > 0) spans.push(`ran ${durationOf(totals.runMs)}`);
  if (totals.backoffMs > 0) spans.push(`backoff ${durationOf(totals.backoffMs)}`);
  if (spans.length > 0) parts.push(spans.join(", "));

  if (lane.state === "QUEUED" && lane.blockedBy.length > 0) {
    parts.push(`blocked by ${lane.blockedBy.join(", ")}`);
  }

  const error = lastError(lane);
  if (error) parts.push(`error ${error.code}: ${error.message}`);

  return `${parts.join(". ")}.`;
}

/** The error of the most recent attempt that had one. A lane that failed, was
 *  retried and then succeeded has no current error and must not announce one. */
function lastError(lane: Lane): ErrorInfo | undefined {
  const last = lane.attempts[lane.attempts.length - 1];
  return last?.error;
}

/**
 * The union of the intervals in which SOMETHING was claimed or executing.
 *
 * The complement of this, inside the chart's domain, is what the compressed
 * scale collapses into gutters. It is computed from `pending` and `run` only:
 * a queued step is not work, and a backoff is by definition a period in which
 * the runtime is deliberately doing nothing, which is exactly the forty
 * minutes that has to collapse for a 3ms step to remain visible beside it.
 */
export function busyIntervals(segments: readonly Segment[]): Array<[number, number]> {
  const spans = segments
    .filter((segment) => segment.kind === "pending" || segment.kind === "run")
    .map((segment): [number, number] => [segment.from, Math.max(segment.to, segment.from)])
    .sort((a, b) => a[0] - b[0]);

  const merged: Array<[number, number]> = [];
  for (const [from, to] of spans) {
    const last = merged[merged.length - 1];
    if (last && from <= last[1]) last[1] = Math.max(last[1], to);
    else merged.push([from, to]);
  }
  return merged;
}

/**
 * The chart's X domain.
 *
 * It starts at the JOB's creation, never at the earliest surviving event. That
 * distinction is the whole point of the truncation band: if the ring evicted
 * the first ten minutes, starting the axis at the oldest event still in hand
 * would silently redraw the job as though it had begun ten minutes later than
 * it did. The axis tells the truth and the missing stretch is hatched.
 *
 * The `max` over segment ends is not redundant with `ended_at`: a worker's
 * clock and this browser's clock are different clocks, and an event stamped a
 * few hundred milliseconds past the job's own end would otherwise be drawn
 * outside the plot.
 */
export function waterfallDomain(
  job: JobResponse,
  segments: readonly Segment[],
  now: number,
): [number, number] {
  const start = parseInstant(job.created_at) ?? now;
  let end = parseInstant(job.ended_at) ?? now;
  for (const segment of segments) if (segment.to > end) end = segment.to;
  // A domain of zero width divides by zero in every scale ever written. One
  // second is an arbitrary floor, and it is arbitrary in the safe direction:
  // it only ever applies to a job with no measurable extent.
  if (end - start < 1000) end = start + 1000;
  return [start, end];
}

/**
 * What the chart can say about the completeness of its own history.
 *
 * `page` is the `{truncated, oldest_seq}` pair from the events endpoint, which
 * the caller must pass through rather than swallow. The reducer adds one check
 * the endpoint cannot make: whether JOB_CREATED is present at all.
 */
export function buildHistory(
  job: JobResponse,
  events: readonly RunMeshEvent[],
  page?: { truncated?: boolean; oldest_seq?: number },
): WaterfallHistory {
  let earliestAt: number | undefined;
  let oldestSeq = page?.oldest_seq ?? 0;
  let sawJobCreated = false;

  for (const event of events) {
    if (event.job_id && event.job_id !== job.id) continue;
    if (event.type === "JOB_CREATED") sawJobCreated = true;
    const at = parseInstant(event.at);
    if (at !== undefined && (earliestAt === undefined || at < earliestAt)) earliestAt = at;
    if (oldestSeq === 0 || event.seq < oldestSeq) oldestSeq = event.seq;
  }

  return {
    truncated: page?.truncated ?? false,
    oldestSeq,
    earliestAt,
    missingJobCreated: events.length > 0 && !sawJobCreated,
  };
}

/** Exported for the axis and the sr-only table, which both need to turn an
 *  epoch number back into a string without importing a second parser. */
export { parseInstant };
