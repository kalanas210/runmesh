import { describe, expect, it } from "vitest";
import {
  buildHistory,
  buildLanes,
  attemptAt,
  busyIntervals,
  depthBands,
  describeLane,
  laneSegments,
  queuedAnchor,
  spanTotals,
  waterfallDomain,
  type Lane,
} from "./waterfall";
import { formatDuration } from "./format";
import type { JobResponse, RunMeshEvent, StepResponse } from "./types";
import { STATE_LABEL, type StepState } from "./state";

/**
 * The reducer is where the correctness of the signature screen lives, so these
 * are behaviour tests against fixed histories, not smoke tests.
 *
 * Every case below is a shape the RUNTIME actually produces and the naive
 * implementation gets wrong, and each one is named after the runtime behaviour
 * rather than after the code path:
 *
 *   - a claim overwrites the step row's timestamps, so multi-attempt history
 *     exists only in the stream;
 *   - a lease expiry and a release NULL those timestamps outright;
 *   - Reconcile emits a bare STEP_FINISHED with no start for a step cancelled
 *     before it ran and for a doomed dependent;
 *   - a cancel can close an attempt that was already closed as RETRYING,
 *     reusing its attempt NUMBER;
 *   - `attempt` and `duration_ms` are omitempty and vanish at zero;
 *   - pages arrive concatenated and a live frame can land out of order;
 *   - the in-memory ring evicts, and the chart must say so.
 *
 * There is no clock in any assertion. `buildLanes` takes none, and the two
 * geometry functions take `now` as a parameter, so every expectation below is
 * an exact number rather than a range.
 */

const T0 = Date.parse("2026-09-12T03:00:00.000Z");

/** An absolute instant, written as an offset from the job's creation. */
function at(offsetMs: number): string {
  return new Date(T0 + offsetMs).toISOString();
}

function makeStep(partial: Partial<StepResponse> & { id: string }): StepResponse {
  return {
    tool: "echo",
    depends_on: [],
    blocked_by: [],
    state: "QUEUED",
    attempt: 0,
    failures: 0,
    max_attempts: 3,
    timeout_seconds: 30,
    scheduled_at: null,
    started_at: null,
    ended_at: null,
    next_attempt_at: null,
    duration_ms: null,
    result: null,
    error: null,
    version: 1,
    ...partial,
  };
}

function makeJob(steps: StepResponse[], partial: Partial<JobResponse> = {}): JobResponse {
  return {
    id: "job_01",
    name: "fixture",
    state: "RUNNING",
    priority: 0,
    on_step_failure: "fail_fast",
    version: 1,
    created_at: at(0),
    updated_at: at(0),
    started_at: null,
    ended_at: null,
    duration_ms: null,
    cancel_requested_at: null,
    error: null,
    steps,
    ...partial,
  };
}

/**
 * An event builder that assigns `seq` in call order, because seq is gap-free
 * and 1-based per job and hand-numbering a twenty-event fixture is how a test
 * ends up asserting against a history the runtime could never emit.
 */
function stream(): {
  push: (event: Omit<RunMeshEvent, "seq" | "global_seq" | "job_id">) => RunMeshEvent;
  all: () => RunMeshEvent[];
} {
  const events: RunMeshEvent[] = [];
  return {
    push(event) {
      const full: RunMeshEvent = {
        ...event,
        seq: events.length + 1,
        global_seq: events.length + 1,
        job_id: "job_01",
      };
      events.push(full);
      return full;
    },
    all: () => events,
  };
}

function laneFor(lanes: Lane[], stepId: string): Lane {
  const lane = lanes.find((entry) => entry.stepId === stepId);
  if (!lane) throw new Error(`no lane for ${stepId}`);
  return lane;
}

/* -------------------------------------------------------------------------- */

describe("buildLanes: a step that retried twice and then succeeded", () => {
  // The snapshot describes attempt 3 and nothing else. claimOneLocked
  // overwrote scheduled_at, nulled started_at and ended_at, and cleared the
  // error on each of the two re-claims, so attempts 1 and 2 exist ONLY here.
  const job = makeJob([
    makeStep({
      id: "fetch",
      tool: "http_request",
      state: "SUCCEEDED",
      attempt: 3,
      failures: 2,
      max_attempts: 3,
      scheduled_at: at(20_400),
      started_at: at(20_500),
      ended_at: at(21_000),
      duration_ms: 500,
    }),
  ]);

  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED", attrs: { steps: 1, name: "fixture" } });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "fetch", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(150), state: "RUNNING", step_id: "fetch", attempt: 1 });
  s.push({
    type: "STEP_RETRY_SCHEDULED",
    at: at(400),
    state: "RETRYING",
    step_id: "fetch",
    attempt: 1,
    duration_ms: 250,
    error: { code: "tool_broke_contract", message: "bad json", retryable: true, attempt: 1 },
    attrs: { next_attempt_at: at(1_400), failures: 1 },
  });
  s.push({ type: "STEP_SCHEDULED", at: at(1_400), state: "SCHEDULED", step_id: "fetch", attempt: 2 });
  s.push({ type: "STEP_STARTED", at: at(1_450), state: "RUNNING", step_id: "fetch", attempt: 2 });
  s.push({
    type: "STEP_RETRY_SCHEDULED",
    at: at(1_700),
    state: "RETRYING",
    step_id: "fetch",
    attempt: 2,
    duration_ms: 250,
    error: { code: "tool_broke_contract", message: "bad json", retryable: true, attempt: 2 },
    // A forty-minute backoff in the same job as a 250ms attempt. This is the
    // pair the compressed scale exists for.
    attrs: { next_attempt_at: at(2_400_000), failures: 2 },
  });
  s.push({ type: "STEP_SCHEDULED", at: at(2_400_000), state: "SCHEDULED", step_id: "fetch", attempt: 3 });
  s.push({ type: "STEP_STARTED", at: at(2_400_100), state: "RUNNING", step_id: "fetch", attempt: 3 });
  s.push({
    type: "STEP_FINISHED",
    at: at(2_400_600),
    state: "SUCCEEDED",
    step_id: "fetch",
    attempt: 3,
    duration_ms: 500,
  });

  const lanes = buildLanes(job, s.all());
  const lane = laneFor(lanes, "fetch");

  it("recovers all three attempts, of which the snapshot remembers one", () => {
    expect(lane.attempts.map((a) => a.n)).toEqual([1, 2, 3]);
    // The proof that this did not come from the snapshot: the snapshot's only
    // start is 20_500, which belongs to none of these.
    expect(lane.attempts[0].startedAt).toBe(T0 + 150);
    expect(lane.attempts[1].startedAt).toBe(T0 + 1_450);
  });

  it("keeps attempt and failures apart", () => {
    // Attempt names the execution and is monotonic; failures is the budget.
    // Attempt 3 succeeded, so two failures were spent across three attempts.
    expect(lane.attempt).toBe(3);
    expect(lane.failures).toBe(2);
    expect(lane.attempts[1].failures).toBe(2);
    expect(lane.attempts[2].failures).toBeUndefined();
  });

  it("reads the backoff out of attrs.next_attempt_at as an instant", () => {
    expect(lane.attempts[0].backoffUntil).toBe(T0 + 1_400);
    expect(lane.attempts[1].backoffUntil).toBe(T0 + 2_400_000);
    expect(lane.attempts[2].backoffUntil).toBeUndefined();
  });

  it("takes duration from the closing event rather than recomputing it", () => {
    expect(lane.attempts[2].durationMs).toBe(500);
    // ended - started is 500 here too, but the reported number is the one the
    // worker measured and the one the API prints elsewhere on the screen.
    expect(lane.attempts[2].endedAt! - lane.attempts[2].startedAt!).toBe(500);
  });

  it("draws a backoff interval between the attempts, not dead space", () => {
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 2_400_600 });
    const backoffs = segments.filter((segment) => segment.kind === "backoff");
    expect(backoffs).toHaveLength(2);
    expect(backoffs[1].to - backoffs[1].from).toBe(2_400_000 - 1_700);
  });

  it("never merges the pending gap into the running bar", () => {
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 2_400_600 });
    const pending = segments.filter((segment) => segment.kind === "pending");
    const run = segments.filter((segment) => segment.kind === "run");
    expect(pending).toHaveLength(3);
    expect(run).toHaveLength(3);
    // The claim-to-start gap is its own interval on every attempt, and
    // duration_ms excludes it. Merging them would make the bar disagree with
    // the number the API prints beside it.
    expect(pending.map((segment) => segment.to - segment.from)).toEqual([50, 50, 100]);
    expect(run.map((segment) => segment.to - segment.from)).toEqual([250, 250, 500]);
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: a lease expiry mid-flight", () => {
  // ExpireLeases nulls ScheduledAt and StartedAt on the way back to QUEUED, so
  // the row loses the span entirely. Only the events remember that a worker
  // held this step for four minutes and then stopped answering.
  const job = makeJob([
    makeStep({ id: "sleep", tool: "sleep", state: "RUNNING", attempt: 2, failures: 1 }),
  ]);

  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "sleep", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(200), state: "RUNNING", step_id: "sleep", attempt: 1 });
  s.push({
    type: "STEP_LEASE_EXPIRED",
    at: at(240_000),
    // The NEW state: QUEUED means requeued, FAILED would mean the budget ran out.
    state: "QUEUED",
    step_id: "sleep",
    attempt: 1,
    attrs: { owner: "worker-7", failures: 1 },
  });
  s.push({ type: "STEP_SCHEDULED", at: at(240_500), state: "SCHEDULED", step_id: "sleep", attempt: 2 });
  s.push({ type: "STEP_STARTED", at: at(240_600), state: "RUNNING", step_id: "sleep", attempt: 2 });

  const lanes = buildLanes(job, s.all());
  const lane = laneFor(lanes, "sleep");

  it("keeps the span the store nulled out", () => {
    expect(lane.attempts[0].startedAt).toBe(T0 + 200);
    expect(lane.attempts[0].endedAt).toBe(T0 + 240_000);
    expect(lane.attempts[0].open).toBe(false);
  });

  it("carries the owner that stopped answering", () => {
    expect(lane.attempts[0].expiredOwner).toBe("worker-7");
    expect(lane.attempts[0].state).toBe("QUEUED");
  });

  it("leaves the replacement attempt open, with its right edge at now", () => {
    expect(lane.attempts[1].open).toBe(true);
    expect(lane.attempts[1].endedAt).toBeUndefined();

    const now = T0 + 300_000;
    const segments = laneSegments(lane, { anchor: T0, now });
    const run = segments.filter((segment) => segment.kind === "run");
    expect(run[1].to).toBe(now);
    expect(run[1].open).toBe(true);
    expect(run[1].state).toBe("RUNNING");
  });

  it("spends retry budget, unlike a release", () => {
    expect(lane.attempts[0].failures).toBe(1);
    expect(lane.attempts[0].releasedReason).toBeUndefined();
  });
});

describe("buildLanes: a release returns a step without spending budget", () => {
  const job = makeJob([makeStep({ id: "work", state: "QUEUED", attempt: 1 })]);
  const s = stream();
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "work", attempt: 1 });
  s.push({
    type: "STEP_RELEASED",
    at: at(900),
    state: "QUEUED",
    step_id: "work",
    attempt: 1,
    attrs: { reason: "shutdown_drain" },
  });

  const lane = laneFor(buildLanes(job, s.all()), "work");

  it("records the reason and no failure", () => {
    expect(lane.attempts[0].releasedReason).toBe("shutdown_drain");
    expect(lane.attempts[0].failures).toBeUndefined();
    expect(lane.attempts[0].state).toBe("QUEUED");
  });

  it("renders as a pending span that never became a run", () => {
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 1_500 });
    expect(segments.filter((segment) => segment.kind === "run")).toHaveLength(0);
    const pending = segments.find((segment) => segment.kind === "pending");
    expect(pending?.to).toBe(T0 + 900);
    // And the lane is waiting again, so the tail is queued time nobody owns.
    const tail = segments[segments.length - 1];
    expect(tail.kind).toBe("queued");
    expect(tail.to).toBe(T0 + 1_500);
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: a cancelled job", () => {
  // Three shapes in one fixture, because a real cancel produces all three at
  // once: a step that was running and was cancelled, a step that never started
  // and was cancelled where it stood, and a step whose attempt had ALREADY
  // closed as RETRYING and is then closed a second time under the same
  // attempt number.
  const job = makeJob(
    [
      makeStep({ id: "a", state: "CANCELLED", attempt: 1 }),
      makeStep({ id: "b", state: "CANCELLED", attempt: 0 }),
      makeStep({ id: "c", state: "CANCELLED", attempt: 1, failures: 1 }),
    ],
    {
      state: "CANCELLED",
      cancel_requested_at: at(5_000),
      cancel_reason: "user",
      ended_at: at(5_000),
    },
  );

  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "a", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(150), state: "RUNNING", step_id: "a", attempt: 1 });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "c", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(150), state: "RUNNING", step_id: "c", attempt: 1 });
  s.push({
    type: "STEP_RETRY_SCHEDULED",
    at: at(900),
    state: "RETRYING",
    step_id: "c",
    attempt: 1,
    duration_ms: 750,
    error: { code: "unclassified", message: "boom", retryable: true, attempt: 1 },
    attrs: { next_attempt_at: at(3_000), failures: 1 },
  });
  s.push({ type: "JOB_CANCEL_REQUESTED", at: at(5_000), state: "RUNNING", attrs: { reason: "user" } });
  s.push({
    type: "STEP_FINISHED",
    at: at(5_000),
    state: "CANCELLED",
    step_id: "a",
    attempt: 1,
    duration_ms: 4_850,
    error: { code: "cancelled", message: "job cancelled", retryable: false, attempt: 1 },
  });
  s.push({
    // `attempt` is omitempty and this step was never claimed, so the field is
    // absent from the wire entirely. A decoder that required it would drop the
    // event and the step would silently vanish from the chart.
    type: "STEP_FINISHED",
    at: at(5_000),
    state: "CANCELLED",
    step_id: "b",
    error: { code: "cancelled", message: "job cancelled", retryable: false, attempt: 0 },
  });
  s.push({
    // The same attempt NUMBER as the retry above: no claim happened in
    // between, so nothing incremented it.
    type: "STEP_FINISHED",
    at: at(5_000),
    state: "CANCELLED",
    step_id: "c",
    attempt: 1,
    error: { code: "cancelled", message: "job cancelled", retryable: false, attempt: 1 },
  });

  const lanes = buildLanes(job, s.all());

  it("closes a running attempt at the cancel instant", () => {
    const lane = laneFor(lanes, "a");
    expect(lane.attempts).toHaveLength(1);
    expect(lane.attempts[0].state).toBe("CANCELLED");
    expect(lane.attempts[0].endedAt).toBe(T0 + 5_000);
    expect(lane.attempts[0].error?.code).toBe("cancelled");
  });

  it("keeps a step that finished with no start, as a terminal tick", () => {
    const lane = laneFor(lanes, "b");
    expect(lane.attempts).toHaveLength(1);
    expect(lane.attempts[0].n).toBe(0);
    expect(lane.attempts[0].neverRan).toBe(true);
    expect(lane.attempts[0].startedAt).toBeUndefined();
    expect(lane.attempts[0].clamped).toBe(true);

    const segments = laneSegments(lane, { anchor: T0, now: T0 + 5_000 });
    const tick = segments.find((segment) => segment.kind === "tick");
    expect(tick).toBeDefined();
    expect(tick!.from).toBe(tick!.to);
    expect(tick!.state).toBe("CANCELLED");
    // And no run segment of zero width, which would be invisible and
    // impossible to hover or focus.
    expect(segments.filter((segment) => segment.kind === "run")).toHaveLength(0);
  });

  it("forks rather than overwriting when a closed attempt is closed again", () => {
    const lane = laneFor(lanes, "c");
    // The retry and the cancellation both name attempt 1. Overwriting would
    // erase the failure and the backoff — the two facts the chart was opened
    // for — and leave a lane that looks as though it was simply cancelled.
    expect(lane.attempts).toHaveLength(2);
    expect(lane.attempts.map((a) => a.n)).toEqual([1, 1]);
    expect(lane.attempts[0].state).toBe("RETRYING");
    expect(lane.attempts[0].durationMs).toBe(750);
    expect(lane.attempts[0].backoffUntil).toBe(T0 + 3_000);
    expect(lane.attempts[1].state).toBe("CANCELLED");
    expect(lane.attempts[1].neverRan).toBe(true);
  });

  it("draws no trailing queued time on a terminal lane", () => {
    for (const lane of lanes) {
      const segments = laneSegments(lane, { anchor: T0, now: T0 + 900_000 });
      expect(segments.every((segment) => segment.to <= T0 + 5_000)).toBe(true);
    }
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: a still-running step", () => {
  const job = makeJob([
    makeStep({ id: "long", tool: "sleep", state: "RUNNING", attempt: 1, started_at: at(200) }),
  ]);
  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "long", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(200), state: "RUNNING", step_id: "long", attempt: 1 });

  const lane = laneFor(buildLanes(job, s.all()), "long");

  it("leaves the attempt open with no end and no duration", () => {
    expect(lane.attempts[0].open).toBe(true);
    expect(lane.attempts[0].endedAt).toBeUndefined();
    expect(lane.attempts[0].durationMs).toBeUndefined();
    // An open attempt is never judged clamped: its extent is whatever `now`
    // makes it, and `now` is not the reducer's business.
    expect(lane.attempts[0].clamped).toBe(false);
  });

  it("moves its right edge with now, and only its right edge", () => {
    const early = laneSegments(lane, { anchor: T0, now: T0 + 1_000 });
    const later = laneSegments(lane, { anchor: T0, now: T0 + 9_000 });
    const earlyRun = early.find((segment) => segment.kind === "run")!;
    const laterRun = later.find((segment) => segment.kind === "run")!;
    expect(earlyRun.from).toBe(laterRun.from);
    expect(earlyRun.to).toBe(T0 + 1_000);
    expect(laterRun.to).toBe(T0 + 9_000);
    expect(laterRun.open).toBe(true);
  });

  it("does not add a trailing queued tail while an attempt is open", () => {
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 9_000 });
    const trailing = segments.filter(
      (segment) => segment.kind === "queued" && segment.to === T0 + 9_000,
    );
    expect(trailing).toHaveLength(0);
  });

  it("survives a start that is ahead of this browser's clock", () => {
    // Worker and browser are different clocks. A negative width is a bar the
    // browser silently drops.
    const segments = laneSegments(lane, { anchor: T0, now: T0 - 5_000 });
    for (const segment of segments) expect(segment.to).toBeGreaterThanOrEqual(segment.from);
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: events that arrive out of order", () => {
  // Pages arrive concatenated and a live frame can land while one is in
  // flight. Folding in arrival order would reopen a closed attempt and leave
  // the step drawn as still running for ever.
  const job = makeJob([makeStep({ id: "x", state: "SUCCEEDED", attempt: 1 })]);

  const ordered: RunMeshEvent[] = [
    { seq: 1, global_seq: 1, job_id: "job_01", type: "JOB_CREATED", at: at(0), state: "QUEUED" },
    { seq: 2, global_seq: 2, job_id: "job_01", type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "x", attempt: 1 },
    { seq: 3, global_seq: 3, job_id: "job_01", type: "STEP_STARTED", at: at(150), state: "RUNNING", step_id: "x", attempt: 1 },
    { seq: 4, global_seq: 4, job_id: "job_01", type: "STEP_FINISHED", at: at(400), state: "SUCCEEDED", step_id: "x", attempt: 1, duration_ms: 250 },
  ];
  const shuffled = [ordered[3], ordered[1], ordered[0], ordered[2]];

  it("folds by seq, not by arrival", () => {
    const a = buildLanes(job, ordered);
    const b = buildLanes(job, shuffled);
    expect(b).toEqual(a);
    expect(laneFor(b, "x").attempts[0].open).toBe(false);
    expect(laneFor(b, "x").attempts[0].state).toBe("SUCCEEDED");
  });

  it("ignores an event belonging to another job", () => {
    const foreign: RunMeshEvent = {
      seq: 2,
      global_seq: 99,
      job_id: "job_02",
      type: "STEP_STARTED",
      at: at(5),
      state: "RUNNING",
      step_id: "x",
      attempt: 7,
    };
    const lanes = buildLanes(job, [...ordered, foreign]);
    expect(laneFor(lanes, "x").attempts).toHaveLength(1);
  });

  it("ignores a step id the snapshot does not know", () => {
    const orphan: RunMeshEvent = {
      seq: 5,
      global_seq: 5,
      job_id: "job_01",
      type: "STEP_STARTED",
      at: at(500),
      state: "RUNNING",
      step_id: "ghost",
      attempt: 1,
    };
    const lanes = buildLanes(job, [...ordered, orphan]);
    expect(lanes).toHaveLength(1);
    expect(laneFor(lanes, "x").attempts).toHaveLength(1);
  });
});

/* -------------------------------------------------------------------------- */

describe("buildHistory: a truncated history", () => {
  // RUNMESH_STORE=memory keeps a fixed-capacity ring per job. On a long or
  // heavily retried job it evicts, and the attempts it evicted are gone. The
  // chart must say so at its left edge rather than redraw the job as though it
  // had started later.
  const job = makeJob([makeStep({ id: "x", state: "SUCCEEDED", attempt: 4 })], {
    created_at: at(0),
  });

  const survivors: RunMeshEvent[] = [
    { seq: 91, global_seq: 91, job_id: "job_01", type: "STEP_SCHEDULED", at: at(600_000), state: "SCHEDULED", step_id: "x", attempt: 4 },
    { seq: 92, global_seq: 92, job_id: "job_01", type: "STEP_STARTED", at: at(600_100), state: "RUNNING", step_id: "x", attempt: 4 },
    { seq: 93, global_seq: 93, job_id: "job_01", type: "STEP_FINISHED", at: at(600_400), state: "SUCCEEDED", step_id: "x", attempt: 4, duration_ms: 300 },
  ];

  it("reports the hole rather than swallowing it", () => {
    const history = buildHistory(job, survivors, { truncated: true, oldest_seq: 91 });
    expect(history.truncated).toBe(true);
    expect(history.oldestSeq).toBe(91);
    expect(history.earliestAt).toBe(T0 + 600_000);
    expect(history.missingJobCreated).toBe(true);
  });

  it("still anchors the axis at the job's creation, not at the oldest survivor", () => {
    const lanes = buildLanes(job, survivors);
    const segments = laneSegments(laneFor(lanes, "x"), { anchor: T0, now: T0 + 600_400 });
    const domain = waterfallDomain(job, segments, T0 + 600_400);
    // Starting at the oldest surviving event would silently redraw this job as
    // though it began ten minutes late.
    expect(domain[0]).toBe(T0);
    expect(domain[1]).toBe(T0 + 600_400);
  });

  it("does not claim a hole when the whole history is present", () => {
    const s = stream();
    s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
    s.push({ type: "STEP_FINISHED", at: at(10), state: "SUCCEEDED", step_id: "x", attempt: 1 });
    const history = buildHistory(job, s.all(), { truncated: false, oldest_seq: 1 });
    expect(history.truncated).toBe(false);
    expect(history.missingJobCreated).toBe(false);
  });

  it("reports nothing missing for a job with no events at all yet", () => {
    const history = buildHistory(job, []);
    expect(history.missingJobCreated).toBe(false);
    expect(history.truncated).toBe(false);
    expect(history.earliestAt).toBeUndefined();
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: a zero-duration step", () => {
  // `echo` returns in under the timestamp resolution, and duration_ms is
  // omitempty so it VANISHES at zero rather than arriving as 0.
  const job = makeJob([
    makeStep({ id: "echo", state: "SUCCEEDED", attempt: 1, duration_ms: 0 }),
  ]);
  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(1_000), state: "SCHEDULED", step_id: "echo", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(1_000), state: "RUNNING", step_id: "echo", attempt: 1 });
  s.push({ type: "STEP_FINISHED", at: at(1_000), state: "SUCCEEDED", step_id: "echo", attempt: 1 });

  const lane = laneFor(buildLanes(job, s.all()), "echo");

  it("flags the attempt as having no measurable extent", () => {
    expect(lane.attempts[0].clamped).toBe(true);
    expect(lane.attempts[0].durationMs).toBeUndefined();
    expect(lane.attempts[0].neverRan).toBe(false);
  });

  it("still produces a run segment, so the step is hoverable and focusable", () => {
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 2_000 });
    const run = segments.find((segment) => segment.kind === "run");
    expect(run).toBeDefined();
    expect(run!.from).toBe(run!.to);
    expect(Number.isFinite(run!.from)).toBe(true);
  });

  it("never produces a NaN or a negative width anywhere in the lane", () => {
    for (const segment of laneSegments(lane, { anchor: T0, now: T0 + 2_000 })) {
      expect(Number.isFinite(segment.from)).toBe(true);
      expect(Number.isFinite(segment.to)).toBe(true);
      expect(segment.to - segment.from).toBeGreaterThanOrEqual(0);
    }
  });
});

describe("buildLanes: timestamps that do not parse", () => {
  const job = makeJob([makeStep({ id: "x", state: "RUNNING", attempt: 1 })]);
  const s = stream();
  s.push({ type: "STEP_SCHEDULED", at: "", state: "SCHEDULED", step_id: "x", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: "not a date", state: "RUNNING", step_id: "x", attempt: 1 });

  it("treats an unparseable instant as absent rather than as NaN", () => {
    // Date.parse("") is NaN, NaN propagates through every arithmetic operation
    // downstream, and the first visible symptom is an element with width="NaN"
    // that the browser drops without a console message.
    const lane = laneFor(buildLanes(job, s.all()), "x");
    expect(lane.attempts[0].scheduledAt).toBeUndefined();
    expect(lane.attempts[0].startedAt).toBeUndefined();
    expect(lane.attempts[0].neverRan).toBe(true);
    for (const segment of laneSegments(lane, { anchor: T0, now: T0 + 100 })) {
      expect(Number.isFinite(segment.from)).toBe(true);
    }
  });
});

/* -------------------------------------------------------------------------- */

describe("buildLanes: a job whose steps all failed", () => {
  // fail_fast: one step fails, the job is cancel-flagged, and Reconcile dooms
  // every dependent with a bare STEP_FINISHED carrying `dependency_failed` and
  // the SAME `now` — a whole column of zero-extent terminal ticks on one
  // instant.
  const steps: StepResponse[] = [
    makeStep({ id: "fetch", state: "FAILED", attempt: 3, failures: 3, max_attempts: 3 }),
    makeStep({ id: "analyze", depends_on: ["fetch"], state: "CANCELLED" }),
    makeStep({ id: "report", tool: "report_generate", depends_on: ["analyze"], state: "CANCELLED" }),
  ];
  const job = makeJob(steps, {
    state: "FAILED",
    ended_at: at(3_000),
    error: { code: "unclassified", message: "fetch failed", retryable: false, attempt: 3 },
  });

  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "fetch", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(120), state: "RUNNING", step_id: "fetch", attempt: 1 });
  s.push({
    type: "STEP_FINISHED",
    at: at(3_000),
    state: "FAILED",
    step_id: "fetch",
    attempt: 1,
    duration_ms: 2_880,
    error: { code: "unclassified", message: "fetch failed", retryable: false, attempt: 1 },
  });
  s.push({ type: "JOB_CANCEL_REQUESTED", at: at(3_000), state: "RUNNING", attrs: { reason: "step_failed", step_id: "fetch" } });
  for (const id of ["analyze", "report"]) {
    s.push({
      type: "STEP_FINISHED",
      at: at(3_000),
      state: "CANCELLED",
      step_id: id,
      error: { code: "dependency_failed", message: "a dependency failed", retryable: false, attempt: 0 },
    });
  }
  s.push({
    type: "JOB_FINISHED",
    at: at(3_000),
    state: "FAILED",
    duration_ms: 2_880,
    error: { code: "unclassified", message: "fetch failed", retryable: false, attempt: 3 },
  });

  const lanes = buildLanes(job, s.all());

  it("bands the lanes by topological depth", () => {
    expect(lanes.map((lane) => lane.depth)).toEqual([0, 1, 2]);
    const bands = depthBands(lanes);
    expect(bands.map((band) => band.depth)).toEqual([0, 1, 2]);
    expect(bands[0].lanes.map((lane) => lane.stepId)).toEqual(["fetch"]);
  });

  it("gives every doomed dependent a terminal tick and no bar", () => {
    for (const id of ["analyze", "report"]) {
      const lane = laneFor(lanes, id);
      expect(lane.attempts[0].neverRan).toBe(true);
      expect(lane.attempts[0].error?.code).toBe("dependency_failed");
      const anchor = queuedAnchor(lane, job, lanes).at;
      const segments = laneSegments(lane, { anchor, now: T0 + 3_000 });
      expect(segments.some((segment) => segment.kind === "tick")).toBe(true);
      expect(segments.some((segment) => segment.kind === "run")).toBe(false);
    }
  });

  it("anchors a dependent's queued bar at its dependency's completion", () => {
    // Nothing in the runtime records when a step became eligible, so this is
    // an inference and the flag says so at every call site.
    const analyze = laneFor(lanes, "analyze");
    const anchor = queuedAnchor(analyze, job, lanes);
    expect(anchor.at).toBe(T0 + 3_000);
    expect(anchor.inferred).toBe(true);

    const fetch = laneFor(lanes, "fetch");
    expect(queuedAnchor(fetch, job, lanes).at).toBe(T0);
  });

  it("marks the first queued segment as inferred and later ones as not", () => {
    const fetch = laneFor(lanes, "fetch");
    const segments = laneSegments(fetch, { anchor: T0, now: T0 + 3_000 });
    const queued = segments.filter((segment) => segment.kind === "queued");
    expect(queued[0].inferred).toBe(true);
  });

  it("lands every dependent's tick on the identical instant without colliding", () => {
    const ticks = lanes
      .flatMap((lane) =>
        laneSegments(lane, { anchor: queuedAnchor(lane, job, lanes).at, now: T0 + 3_000 }),
      )
      .filter((segment) => segment.kind === "tick");
    expect(ticks).toHaveLength(2);
    expect(new Set(ticks.map((tick) => tick.from))).toEqual(new Set([T0 + 3_000]));
  });
});

/* -------------------------------------------------------------------------- */

describe("depth", () => {
  it("uses the longest path, so a band means 'these could run together'", () => {
    // b depends on a (depth 1). d depends on a AND c, and c depends on b, so
    // the longest path to d is a->b->c->d and d belongs in band 3. Shortest
    // path would put it in band 1, below a band that has not started.
    const job = makeJob([
      makeStep({ id: "a" }),
      makeStep({ id: "b", depends_on: ["a"] }),
      makeStep({ id: "c", depends_on: ["b"] }),
      makeStep({ id: "d", depends_on: ["a", "c"] }),
    ]);
    const lanes = buildLanes(job, []);
    expect(lanes.map((lane) => lane.depth)).toEqual([0, 1, 2, 3]);
  });

  it("terminates on a cycle the API should never have emitted", () => {
    const job = makeJob([
      makeStep({ id: "a", depends_on: ["b"] }),
      makeStep({ id: "b", depends_on: ["a"] }),
    ]);
    // Plan.Validate rejects cycles at submit time, so this payload is not what
    // it claims to be. A slightly wrong band is recoverable; a recursion that
    // never returns takes the tab with it.
    expect(() => buildLanes(job, [])).not.toThrow();
  });

  it("ignores a dependency id that names no step", () => {
    const job = makeJob([makeStep({ id: "a", depends_on: ["nope"] })]);
    expect(buildLanes(job, [])[0].depth).toBe(0);
  });
});

describe("structure comes from the snapshot, not from the stream", () => {
  it("draws a lane for a step that has emitted no events at all", () => {
    const job = makeJob([
      makeStep({ id: "a", state: "SUCCEEDED" }),
      makeStep({ id: "b", depends_on: ["a"], state: "QUEUED", blocked_by: ["a"] }),
    ]);
    const lanes = buildLanes(job, []);
    expect(lanes).toHaveLength(2);
    expect(laneFor(lanes, "b").attempts).toHaveLength(0);
    expect(laneFor(lanes, "b").blockedBy).toEqual(["a"]);
  });

  it("preserves the API's step order, which is plan ordinal and is not on the wire", () => {
    const job = makeJob([
      makeStep({ id: "zeta" }),
      makeStep({ id: "alpha" }),
      makeStep({ id: "mid" }),
    ]);
    expect(buildLanes(job, []).map((lane) => lane.stepId)).toEqual(["zeta", "alpha", "mid"]);
    expect(buildLanes(job, []).map((lane) => lane.index)).toEqual([0, 1, 2]);
  });

  it("converts timeout_seconds, the one duration on this API that is not ms", () => {
    const job = makeJob([makeStep({ id: "a", timeout_seconds: 2.5 })]);
    expect(buildLanes(job, [])[0].timeoutMs).toBe(2_500);
  });

  it("gives a never-claimed lane an open queued segment ending at now", () => {
    const job = makeJob([makeStep({ id: "a", state: "QUEUED" })]);
    const lane = buildLanes(job, [])[0];
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 4_000 });
    expect(segments).toHaveLength(1);
    expect(segments[0]).toMatchObject({ kind: "queued", from: T0, to: T0 + 4_000, inferred: true });
  });
});

/* -------------------------------------------------------------------------- */

describe("busyIntervals", () => {
  it("counts only pending and run, never queued or backoff", () => {
    const job = makeJob([makeStep({ id: "x", state: "SUCCEEDED", attempt: 1 })]);
    const s = stream();
    s.push({ type: "STEP_SCHEDULED", at: at(1_000), state: "SCHEDULED", step_id: "x", attempt: 1 });
    s.push({ type: "STEP_STARTED", at: at(1_100), state: "RUNNING", step_id: "x", attempt: 1 });
    s.push({
      type: "STEP_RETRY_SCHEDULED",
      at: at(1_200),
      state: "RETRYING",
      step_id: "x",
      attempt: 1,
      attrs: { next_attempt_at: at(120_000), failures: 1 },
    });
    const lane = laneFor(buildLanes(job, s.all()), "x");
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 130_000 });
    // The forty-minute-shaped backoff and the queued lead-in are exactly the
    // stretches the compressed scale has to collapse, so they are not busy.
    expect(busyIntervals(segments)).toEqual([[T0 + 1_000, T0 + 1_200]]);
  });

  it("merges overlapping work across lanes into one span", () => {
    const job = makeJob([
      makeStep({ id: "a", state: "SUCCEEDED", attempt: 1 }),
      makeStep({ id: "b", state: "SUCCEEDED", attempt: 1 }),
    ]);
    const s = stream();
    s.push({ type: "STEP_STARTED", at: at(0), state: "RUNNING", step_id: "a", attempt: 1 });
    s.push({ type: "STEP_FINISHED", at: at(500), state: "SUCCEEDED", step_id: "a", attempt: 1 });
    s.push({ type: "STEP_STARTED", at: at(400), state: "RUNNING", step_id: "b", attempt: 1 });
    s.push({ type: "STEP_FINISHED", at: at(900), state: "SUCCEEDED", step_id: "b", attempt: 1 });
    const lanes = buildLanes(job, s.all());
    const segments = lanes.flatMap((lane) => laneSegments(lane, { anchor: T0, now: T0 + 900 }));
    expect(busyIntervals(segments)).toEqual([[T0, T0 + 900]]);
  });

  it("is empty for a job in which nothing ever ran", () => {
    const job = makeJob([makeStep({ id: "a", state: "CANCELLED" })]);
    const s = stream();
    s.push({ type: "STEP_FINISHED", at: at(100), state: "CANCELLED", step_id: "a" });
    const lane = laneFor(buildLanes(job, s.all()), "a");
    expect(busyIntervals(laneSegments(lane, { anchor: T0, now: T0 + 100 }))).toEqual([]);
  });
});

describe("waterfallDomain", () => {
  const job = makeJob([makeStep({ id: "a" })]);

  it("never returns a zero-width domain", () => {
    const [from, to] = waterfallDomain(job, [], T0);
    expect(to).toBeGreaterThan(from);
  });

  it("extends past the job's end for an event stamped by a skewed worker clock", () => {
    const skewed = makeJob([makeStep({ id: "a" })], { ended_at: at(1_000) });
    const [, to] = waterfallDomain(
      skewed,
      [
        {
          kind: "run" as const,
          attempt: 1,
          from: T0 + 900,
          to: T0 + 1_400,
          state: "SUCCEEDED" as StepState,
          open: false,
          inferred: false,
        },
      ],
      T0 + 1_000,
    );
    expect(to).toBe(T0 + 1_400);
  });
});

/* -------------------------------------------------------------------------- */

describe("spanTotals", () => {
  it("keeps the three spans apart, because they mean three different things", () => {
    // queued is the runtime not having claimed the step, pending is a worker
    // holding a lease with the tool not yet running (the pod-pending gap), and
    // run is the tool executing. Summing any two of them answers a question
    // nobody asked.
    const segments = [
      { kind: "queued" as const, attempt: 1, from: T0, to: T0 + 100, state: "QUEUED" as StepState, open: false, inferred: true },
      { kind: "pending" as const, attempt: 1, from: T0 + 100, to: T0 + 400, state: "SCHEDULED" as StepState, open: false, inferred: false },
      { kind: "run" as const, attempt: 1, from: T0 + 400, to: T0 + 900, state: "SUCCEEDED" as StepState, open: false, inferred: false },
      { kind: "backoff" as const, attempt: 1, from: T0 + 900, to: T0 + 2_900, state: "RETRYING" as StepState, open: false, inferred: false },
    ];
    expect(spanTotals(segments)).toEqual({
      queuedMs: 100,
      pendingMs: 300,
      runMs: 500,
      backoffMs: 2_000,
      inferred: true,
    });
  });

  it("does not bank a terminal tick as time, because it has no extent", () => {
    const totals = spanTotals([
      {
        kind: "tick" as const,
        attempt: 0,
        from: T0 + 50,
        to: T0 + 50,
        state: "CANCELLED" as StepState,
        open: false,
        inferred: false,
      },
    ]);
    expect(totals).toEqual({
      queuedMs: 0,
      pendingMs: 0,
      runMs: 0,
      backoffMs: 0,
      inferred: false,
    });
  });

  it("reports an inferred left edge even when a later queued span is recorded", () => {
    const totals = spanTotals([
      { kind: "queued" as const, attempt: 1, from: T0, to: T0 + 10, state: "QUEUED" as StepState, open: false, inferred: true },
      { kind: "queued" as const, attempt: 2, from: T0 + 20, to: T0 + 30, state: "QUEUED" as StepState, open: false, inferred: false },
    ]);
    expect(totals.inferred).toBe(true);
  });
});

describe("describeLane: the chart for a reader who has no chart", () => {
  const label = (state: StepState) => STATE_LABEL[state];
  const duration = (ms: number) => formatDuration(ms);

  it("speaks the state, the attempt count and where the time went", () => {
    const job = makeJob([
      makeStep({ id: "fetch", tool: "http_request", state: "SUCCEEDED", attempt: 1 }),
    ]);
    const events = stream();
    events.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
    events.push({ type: "STEP_SCHEDULED", step_id: "fetch", attempt: 1, at: at(100), state: "SCHEDULED" });
    events.push({ type: "STEP_STARTED", step_id: "fetch", attempt: 1, at: at(400), state: "RUNNING" });
    events.push({ type: "STEP_FINISHED", step_id: "fetch", attempt: 1, at: at(900), state: "SUCCEEDED", duration_ms: 500 });

    const lanes = buildLanes(job, events.all());
    const lane = laneFor(lanes, "fetch");
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 900 });

    expect(describeLane(lane, segments, label, duration)).toBe(
      "fetch, tool http_request, depth 0, SUCCEEDED. 1 attempt. queued 100ms (inferred), pending 300ms, ran 500ms.",
    );
  });

  it("never claims more attempt history than survived the ring", () => {
    // The row says attempt 4. The ring kept one. Announcing "4 attempts" would
    // promise three bars a reader will never find.
    const job = makeJob([makeStep({ id: "flaky", state: "RUNNING", attempt: 4 })]);
    const events = stream();
    events.push({ type: "STEP_SCHEDULED", step_id: "flaky", attempt: 4, at: at(10_000), state: "SCHEDULED" });
    events.push({ type: "STEP_STARTED", step_id: "flaky", attempt: 4, at: at(10_100), state: "RUNNING" });

    const lanes = buildLanes(job, events.all());
    const lane = laneFor(lanes, "flaky");
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 10_600 });

    expect(describeLane(lane, segments, label, duration)).toContain(
      "1 attempt recorded of 4",
    );
  });

  it("says why a queued step has not started, from blocked_by", () => {
    const job = makeJob([
      makeStep({ id: "a", state: "RUNNING" }),
      makeStep({ id: "b", state: "QUEUED", depends_on: ["a"], blocked_by: ["a"] }),
    ]);
    const lanes = buildLanes(job, []);
    const lane = laneFor(lanes, "b");
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 5_000 });
    expect(describeLane(lane, segments, label, duration)).toContain("blocked by a");
  });

  it("announces the last attempt's error and not an error it recovered from", () => {
    const job = makeJob([
      makeStep({ id: "flaky", state: "SUCCEEDED", attempt: 2, failures: 1 }),
    ]);
    const events = stream();
    events.push({ type: "STEP_SCHEDULED", step_id: "flaky", attempt: 1, at: at(0), state: "SCHEDULED" });
    events.push({ type: "STEP_STARTED", step_id: "flaky", attempt: 1, at: at(10), state: "RUNNING" });
    events.push({
      type: "STEP_RETRY_SCHEDULED",
      step_id: "flaky",
      attempt: 1,
      at: at(20),
      state: "RETRYING",
      error: { code: "step_timeout", message: "deadline exceeded", retryable: true, attempt: 1 },
      attrs: { next_attempt_at: at(1_020), failures: 1 },
    });
    events.push({ type: "STEP_SCHEDULED", step_id: "flaky", attempt: 2, at: at(1_020), state: "SCHEDULED" });
    events.push({ type: "STEP_STARTED", step_id: "flaky", attempt: 2, at: at(1_030), state: "RUNNING" });
    events.push({ type: "STEP_FINISHED", step_id: "flaky", attempt: 2, at: at(1_100), state: "SUCCEEDED", duration_ms: 70 });

    const lanes = buildLanes(job, events.all());
    const lane = laneFor(lanes, "flaky");
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 1_100 });
    const sentence = describeLane(lane, segments, label, duration);

    expect(sentence).toContain("2 attempts");
    expect(sentence).toContain("backoff");
    expect(sentence).not.toContain("step_timeout");
  });

  it("says a lane has no attempts rather than saying nothing", () => {
    const job = makeJob([makeStep({ id: "later", state: "QUEUED" })]);
    const lanes = buildLanes(job, []);
    const lane = laneFor(lanes, "later");
    expect(describeLane(lane, [], label, duration)).toBe(
      "later, tool echo, depth 0, QUEUED. no attempts recorded.",
    );
  });
});

describe("attemptAt: two attempts can share a number", () => {
  // The runtime shape: a step fails and goes RETRYING as attempt 1, then the
  // job is cancelled before the backoff elapses. Reconcile stamps a bare
  // STEP_FINISHED with state CANCELLED carrying attempt 1 again, because no
  // claim happened in between to increment it.
  const job = makeJob([makeStep({ id: "flaky", state: "CANCELLED", attempt: 1, failures: 1 })]);
  const events = stream();
  events.push({ type: "STEP_SCHEDULED", step_id: "flaky", attempt: 1, at: at(0), state: "SCHEDULED" });
  events.push({ type: "STEP_STARTED", step_id: "flaky", attempt: 1, at: at(10), state: "RUNNING" });
  events.push({
    type: "STEP_RETRY_SCHEDULED",
    step_id: "flaky",
    attempt: 1,
    at: at(50),
    state: "RETRYING",
    error: { code: "unclassified", message: "boom", retryable: true, attempt: 1 },
    attrs: { next_attempt_at: at(2_050), failures: 1 },
  });
  events.push({
    type: "STEP_FINISHED",
    step_id: "flaky",
    attempt: 1,
    at: at(900),
    state: "CANCELLED",
    error: { code: "cancelled", message: "job cancelled", retryable: false, attempt: 1 },
  });

  const lane = laneFor(buildLanes(job, events.all()), "flaky");
  const segments = laneSegments(lane, { anchor: T0, now: T0 + 900 });

  it("attributes the run bar to the attempt that actually ran", () => {
    const run = segments.find((segment) => segment.kind === "run");
    expect(run).toBeDefined();
    expect(attemptAt(lane, run!)?.state).toBe("RETRYING");
  });

  it("attributes the terminal tick to the cancellation, not to the failure", () => {
    const tick = segments.find((segment) => segment.kind === "tick");
    expect(tick).toBeDefined();
    const owner = attemptAt(lane, tick!);
    expect(owner?.state).toBe("CANCELLED");
    expect(owner?.error?.code).toBe("cancelled");
  });

  it("returns undefined rather than guessing for a number no attempt carries", () => {
    expect(
      attemptAt(lane, {
        kind: "run",
        attempt: 9,
        from: T0,
        to: T0 + 1,
        state: "RUNNING",
        open: false,
        inferred: false,
      }),
    ).toBeUndefined();
  });
});

/* -------------------------------------------------------------------------- */

describe("laneSegments: the snapshot lags the stream, and the tail must not", () => {
  /**
   * The two sources move at different speeds and this is the shape that comes
   * out of the gap. STEP_FINISHED arrives over SSE the instant it is emitted;
   * `lane.state` comes from the job row, which is polled every 5000ms while
   * the stream is healthy (feed.ts, jobPollMs). So for up to five seconds the
   * events say SUCCEEDED and the snapshot still says RUNNING.
   *
   * Gated on the snapshot alone, the chart drew a queued bar growing to the
   * right off a step that had already finished — and it grew for as long as
   * the stale row survived, which on a slow poll is most of a chart's width.
   */
  const staleRow = makeJob([
    makeStep({
      id: "fetch",
      state: "RUNNING",
      attempt: 1,
      scheduled_at: at(100),
      started_at: at(200),
    }),
  ]);
  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "fetch", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(200), state: "RUNNING", step_id: "fetch", attempt: 1 });
  s.push({
    type: "STEP_FINISHED",
    at: at(700),
    state: "SUCCEEDED",
    step_id: "fetch",
    attempt: 1,
    duration_ms: 500,
  });

  const lane = laneFor(buildLanes(staleRow, s.all()), "fetch");

  it("draws no phantom queued tail off a step the stream says has finished", () => {
    // Four seconds after the step ended, with a snapshot that has not caught
    // up. The old gate read `!TERMINAL.has(lane.state)` and nothing else.
    const segments = laneSegments(lane, { anchor: T0, now: T0 + 4_700 });
    const phantom = segments.filter(
      (segment) => segment.kind === "queued" && segment.to === T0 + 4_700,
    );
    expect(phantom).toEqual([]);
    // And nothing at all is drawn past the last event.
    expect(segments.every((segment) => segment.to <= T0 + 700)).toBe(true);
  });

  it("does not grow as the stale row survives another poll", () => {
    // The tell that this was a phantom and not a late render: its width was a
    // function of how long the snapshot stayed wrong.
    const early = spanTotals(laneSegments(lane, { anchor: T0, now: T0 + 800 }));
    const later = spanTotals(laneSegments(lane, { anchor: T0, now: T0 + 5_600 }));
    expect(later.queuedMs).toBe(early.queuedMs);
    expect(later.runMs).toBe(early.runMs);
  });

  it("still draws the tail for an attempt closed in a NON-terminal state", () => {
    // The gate keys off the attempt's state, not merely off its being closed,
    // and that distinction is the whole of it. A release closes an attempt as
    // QUEUED and the step really is waiting for its next claim; suppressing
    // that tail would hide genuine queued time behind a fix for a phantom.
    const job = makeJob([makeStep({ id: "drained", state: "QUEUED", attempt: 1 })]);
    const r = stream();
    r.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
    r.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "drained", attempt: 1 });
    r.push({ type: "STEP_STARTED", at: at(200), state: "RUNNING", step_id: "drained", attempt: 1 });
    r.push({
      type: "STEP_RELEASED",
      at: at(300),
      state: "QUEUED",
      step_id: "drained",
      attempt: 1,
      attrs: { reason: "worker draining" },
    });

    const released = laneFor(buildLanes(job, r.all()), "drained");
    const segments = laneSegments(released, { anchor: T0, now: T0 + 4_000 });
    const tail = segments[segments.length - 1];
    expect(tail.kind).toBe("queued");
    expect(tail.open).toBe(true);
    expect(tail.to).toBe(T0 + 4_000);
  });
});

/* -------------------------------------------------------------------------- */

describe("laneSegments: a step inside its backoff right now", () => {
  /**
   * `next_attempt_at` is the one instant on this chart that arrives from the
   * wire as a PREDICTION. The step is not claimable until the clock passes it
   * — there is no timer, only the comparison — so while the backoff is running
   * the number is in the future, and every other in-progress interval in
   * laneSegments is clamped to `now`.
   */
  const job = makeJob([
    makeStep({
      id: "flaky",
      state: "RETRYING",
      attempt: 1,
      failures: 1,
      next_attempt_at: at(600_500),
    }),
  ]);
  const s = stream();
  s.push({ type: "JOB_CREATED", at: at(0), state: "QUEUED" });
  s.push({ type: "STEP_SCHEDULED", at: at(100), state: "SCHEDULED", step_id: "flaky", attempt: 1 });
  s.push({ type: "STEP_STARTED", at: at(200), state: "RUNNING", step_id: "flaky", attempt: 1 });
  s.push({
    type: "STEP_RETRY_SCHEDULED",
    at: at(500),
    state: "RETRYING",
    step_id: "flaky",
    attempt: 1,
    duration_ms: 300,
    error: { code: "unclassified", message: "boom", retryable: true, attempt: 1 },
    // Ten minutes of backoff, of which one second has elapsed at `now`.
    attrs: { next_attempt_at: at(600_500), failures: 1 },
  });

  const lane = laneFor(buildLanes(job, s.all()), "flaky");
  const NOW = T0 + 1_500;

  it("never draws past now", () => {
    const segments = laneSegments(lane, { anchor: T0, now: NOW });
    for (const segment of segments) expect(segment.to).toBeLessThanOrEqual(NOW);
  });

  it("clamps the backoff bar to now and marks it open", () => {
    const backoff = laneSegments(lane, { anchor: T0, now: NOW }).find(
      (segment) => segment.kind === "backoff",
    );
    expect(backoff).toBeDefined();
    expect(backoff!.from).toBe(T0 + 500);
    expect(backoff!.to).toBe(NOW);
    // The right edge is `now` and moves, which is what `open` means and what
    // earns the shimmer the run and pending bars get.
    expect(backoff!.open).toBe(true);
  });

  it("counts only the elapsed backoff, not the promised one", () => {
    // The bug the totals showed: ten minutes of waiting attributed one second
    // into the wait, printed in the hover card as fact.
    const totals = spanTotals(laneSegments(lane, { anchor: T0, now: NOW }));
    expect(totals.backoffMs).toBe(1_000);
  });

  it("grows with now, and settles at the true instant once it elapses", () => {
    const half = laneSegments(lane, { anchor: T0, now: T0 + 300_500 }).find(
      (segment) => segment.kind === "backoff",
    )!;
    expect(half.to).toBe(T0 + 300_500);
    expect(half.open).toBe(true);

    // Past the backoff: the prediction is now history and the bar stops at the
    // instant itself rather than at `now`, because the wait genuinely ended
    // there.
    const elapsed = laneSegments(lane, { anchor: T0, now: T0 + 700_000 }).find(
      (segment) => segment.kind === "backoff",
    )!;
    expect(elapsed.to).toBe(T0 + 600_500);
    expect(elapsed.open).toBe(false);
    expect(spanTotals(laneSegments(lane, { anchor: T0, now: T0 + 700_000 })).backoffMs).toBe(
      600_000,
    );
  });

  it("keeps the whole promised instant on the attempt, for the hover card", () => {
    // Clamping is a fact about the DRAWN extent. `backoff until` in
    // segment-popover.tsx reads the attempt, and it must still be able to say
    // when the next claim becomes possible.
    expect(lane.attempts[0].backoffUntil).toBe(T0 + 600_500);
  });

  it("does not also draw a trailing queued tail over the same interval", () => {
    // The backoff already owns this time. A tail on top of it would double the
    // queued total and paint two bars over one wait.
    const segments = laneSegments(lane, { anchor: T0, now: NOW });
    expect(segments.filter((segment) => segment.kind === "queued" && segment.to === NOW)).toEqual(
      [],
    );
  });
});
