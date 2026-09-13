import { JOB_STATES, TERMINAL, type JobState } from "./state";
import type { JobResponse } from "./types";

/**
 * The numbers the jobs index prints above its table, and the honest caveat
 * that has to travel with every one of them.
 *
 * ---------------------------------------------------------------------------
 * THIS SUMMARISES A PAGE, NOT A FLEET, AND THE UI MUST SAY SO.
 *
 * `GET /api/v1/jobs` is cursor-paginated in id-descending order with NO total
 * count and no aggregate of any kind. There is no metrics route that counts
 * jobs by state — /api/v1/metrics is the process's own exposition (requests,
 * latencies, worker gauges), not a job census — and `engine.Stats()` has no
 * caller. So the only material a stat band can be built from is the rows that
 * happen to be on screen.
 *
 * Which makes the tempting version of this function a fabrication: "4 FAILED"
 * printed as a fleet fact, computed from the fifty most recent jobs, is wrong
 * by an unbounded amount the moment there are fifty-one. The band therefore
 * reports `total` alongside every count and the component labels the whole
 * region with the page's own extent, so the number is true about something the
 * reader can see. The instantaneous fleet numbers — workers, inflight, queue
 * depth — come from /ready and are rendered as a separate group, because they
 * are a different kind of claim about a different population.
 * ---------------------------------------------------------------------------
 *
 * Pure, so the one arithmetic trap here is testable: the failure rate is a
 * ratio whose denominator is legitimately zero on a runtime where nothing has
 * finished yet, and `0/0` renders as "NaN%" on the emptiest screen in the
 * product — the one a first-time reader sees.
 */

/** An hour. The window the "finished recently" count uses. */
export const RECENT_WINDOW_MS = 3_600_000;

export interface PageSummary {
  /** Rows in hand. Every other number below is out of this one. */
  total: number;
  byState: Record<JobState, number>;
  /** QUEUED or RUNNING: work the runtime still owes an answer for. */
  active: number;
  /** Reached an absorbing state. */
  terminal: number;
  /**
   * FAILED or TIMED_OUT. CANCELLED is deliberately NOT counted here: a
   * cancelled job did what it was told, and folding operator intent into a
   * failure count is how a dashboard manufactures an incident out of a
   * deliberate stop.
   */
  failed: number;
  /** failed / terminal, or null when nothing has finished. Never NaN. */
  failureRate: number | null;
  /** Terminal jobs whose `ended_at` falls inside `windowMs` of `now`. */
  finishedRecently: number;
  windowMs: number;
}

function emptyCounts(): Record<JobState, number> {
  // Built from JOB_STATES rather than written out, so a state added to the
  // domain cannot leave a hole here that reads as a zero.
  return Object.fromEntries(JOB_STATES.map((state) => [state, 0])) as Record<JobState, number>;
}

export function summarisePage(
  jobs: readonly JobResponse[],
  now: number,
  windowMs: number = RECENT_WINDOW_MS,
): PageSummary {
  const byState = emptyCounts();
  let terminal = 0;
  let failed = 0;
  let finishedRecently = 0;

  for (const job of jobs) {
    // A state the client does not know about would otherwise increment
    // `undefined`. ParseState makes that impossible on the wire today, but a
    // count that turns into NaN because the API grew a value is a poor way to
    // find out that it did.
    if (byState[job.state] === undefined) continue;
    byState[job.state] += 1;

    if (TERMINAL.has(job.state)) {
      terminal += 1;
      if (job.state === "FAILED" || job.state === "TIMED_OUT") failed += 1;

      const ended = job.ended_at ? Date.parse(job.ended_at) : NaN;
      if (Number.isFinite(ended) && now - ended <= windowMs && now - ended >= 0) {
        finishedRecently += 1;
      }
    }
  }

  return {
    total: jobs.length,
    byState,
    active: byState.QUEUED + byState.RUNNING,
    terminal,
    failed,
    // The guard is the whole reason this is a function. An idle runtime has
    // terminal === 0, and the obvious expression puts "NaN%" on the first
    // screen anybody ever sees.
    failureRate: terminal === 0 ? null : failed / terminal,
    finishedRecently,
    windowMs,
  };
}

/**
 * The rate as a string, or the em dash when there is no rate to state.
 *
 * Whole percentages. A second decimal place on a ratio whose denominator is
 * often under ten implies a precision the sample cannot carry.
 */
export function formatRate(rate: number | null): string {
  if (rate === null) return "—";
  return `${Math.round(rate * 100)}%`;
}
