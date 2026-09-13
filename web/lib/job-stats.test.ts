import { describe, expect, it } from "vitest";
import { formatRate, summarisePage } from "./job-stats";
import type { JobResponse } from "./types";
import type { JobState } from "./state";

const NOW = Date.parse("2026-09-12T12:00:00Z");

function job(state: JobState, endedMinutesAgo?: number): JobResponse {
  const ended =
    endedMinutesAgo === undefined ? null : new Date(NOW - endedMinutesAgo * 60_000).toISOString();
  return {
    id: `job_${state}_${endedMinutesAgo ?? "open"}`,
    name: "n",
    state,
    priority: 0,
    on_step_failure: "fail_fast",
    version: 1,
    created_at: "2026-09-12T11:00:00Z",
    updated_at: "2026-09-12T11:30:00Z",
    started_at: "2026-09-12T11:00:01Z",
    ended_at: ended,
    duration_ms: null,
    cancel_requested_at: null,
    error: null,
    steps: [],
  };
}

describe("summarisePage", () => {
  it("returns a null failure rate rather than NaN when nothing has finished", () => {
    // The denominator is legitimately zero on a runtime nobody has used yet,
    // and 0/0 renders as "NaN%" on the first screen a new reader ever sees.
    const summary = summarisePage([job("QUEUED"), job("RUNNING")], NOW);
    expect(summary.failureRate).toBeNull();
    expect(formatRate(summary.failureRate)).toBe("—");
  });

  it("counts FAILED and TIMED_OUT as failures and CANCELLED as neither", () => {
    // A cancelled job did what it was told. Folding operator intent into a
    // failure count manufactures an incident out of a deliberate stop.
    const summary = summarisePage(
      [job("FAILED", 1), job("TIMED_OUT", 1), job("CANCELLED", 1), job("SUCCEEDED", 1)],
      NOW,
    );
    expect(summary.terminal).toBe(4);
    expect(summary.failed).toBe(2);
    expect(summary.failureRate).toBe(0.5);
    expect(formatRate(summary.failureRate)).toBe("50%");
  });

  it("counts QUEUED and RUNNING as active", () => {
    const summary = summarisePage([job("QUEUED"), job("RUNNING"), job("SUCCEEDED", 1)], NOW);
    expect(summary.active).toBe(2);
    expect(summary.byState.QUEUED).toBe(1);
    expect(summary.byState.RUNNING).toBe(1);
  });

  it("counts only terminal jobs that ended inside the window", () => {
    const summary = summarisePage([job("SUCCEEDED", 10), job("SUCCEEDED", 120)], NOW);
    expect(summary.terminal).toBe(2);
    expect(summary.finishedRecently).toBe(1);
  });

  it("ignores a job whose ended_at is in the future rather than counting it as recent", () => {
    // Two clocks: the runtime's and this browser's. A job stamped a few seconds
    // ahead is normal, and clamping it into the window would be harmless — but
    // a machine an hour out would otherwise inflate "finished in the last hour"
    // with jobs that finished at a time that has not happened here yet.
    const summary = summarisePage([job("SUCCEEDED", -30)], NOW);
    expect(summary.finishedRecently).toBe(0);
  });

  it("gives every job state a zero rather than leaving it undefined", () => {
    const summary = summarisePage([], NOW);
    expect(summary.byState).toEqual({
      QUEUED: 0,
      RUNNING: 0,
      SUCCEEDED: 0,
      FAILED: 0,
      CANCELLED: 0,
      TIMED_OUT: 0,
    });
    expect(summary.total).toBe(0);
  });

  it("survives a state this build has never heard of", () => {
    // ParseState makes it impossible on the wire today. A count that turns into
    // NaN is a poor way to discover that it no longer is.
    const rogue = { ...job("QUEUED"), state: "PARKED" as JobState };
    const summary = summarisePage([rogue, job("RUNNING")], NOW);
    expect(summary.active).toBe(1);
    expect(Number.isFinite(summary.active)).toBe(true);
  });
});
