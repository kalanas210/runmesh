import { describe, expect, it } from "vitest";
import {
  ACTIVE,
  JOB_STATES,
  STATE_FILL_VAR,
  STATE_GLYPH,
  STATE_LABEL,
  STATE_TEXTURE,
  STATE_TONE,
  STATE_VAR,
  STEP_STATES,
  TERMINAL,
  isCancelling,
  type StepState,
} from "./state";

/**
 * These are completeness invariants, not behaviour tests, and they earn their
 * place because of how this design system fails.
 *
 * A state missing from one of the six maps below does not throw and does not
 * warn. `STATE_TONE[state]` is undefined, cn() drops it, and the pill renders
 * with no colour and no stroke — which looks like a deliberate neutral badge.
 * The same silence applies to a missing `@theme inline` entry in globals.css.
 * So the one thing worth asserting mechanically is that every map covers every
 * state, because that is the failure a screenshot does not reveal and a reader
 * would have to know the palette by heart to spot.
 */

const MAPS = {
  STATE_TONE,
  STATE_LABEL,
  STATE_GLYPH,
  STATE_TEXTURE,
  STATE_VAR,
  STATE_FILL_VAR,
} as const;

describe("the state vocabulary", () => {
  it.each(Object.keys(MAPS))("%s covers all eight step states", (name) => {
    const map = MAPS[name as keyof typeof MAPS] as Record<string, unknown>;
    expect(Object.keys(map).sort()).toEqual([...STEP_STATES].sort());
    for (const state of STEP_STATES) {
      expect(map[state], `${name} has no entry for ${state}`).toBeTruthy();
    }
  });

  it("never includes UNKNOWN, which ParseState rejects as a wire value", () => {
    expect(STEP_STATES as readonly string[]).not.toContain("UNKNOWN");
    expect(JOB_STATES as readonly string[]).not.toContain("UNKNOWN");
  });

  it("gives a job six states, omitting SCHEDULED and RETRYING", () => {
    // jobEdges (state.go:119-122) produces neither: job SCHEDULED is reserved
    // with no writer, and job RETRYING is never rolled up. A shared eight-value
    // legend pointed at job.state would carry two entries that can never light.
    expect(JOB_STATES).toHaveLength(6);
    expect(JOB_STATES as readonly string[]).not.toContain("SCHEDULED");
    expect(JOB_STATES as readonly string[]).not.toContain("RETRYING");
    for (const state of JOB_STATES) {
      expect(STEP_STATES as readonly string[]).toContain(state);
    }
  });

  it("matches State.Terminal() and State.Active() exactly", () => {
    expect([...TERMINAL].sort()).toEqual(
      ["CANCELLED", "FAILED", "SUCCEEDED", "TIMED_OUT"],
    );
    expect([...ACTIVE].sort()).toEqual(["RUNNING", "SCHEDULED"]);
    // The two sets are disjoint: a lease is held, or the step has settled.
    for (const state of ACTIVE) {
      expect(TERMINAL.has(state)).toBe(false);
    }
  });

  it("keeps RUNNING the only filled pill", () => {
    // The filled pill is what makes "what is running right now" findable in
    // peripheral vision. A second filled tone would spend that distinction.
    const filled = STEP_STATES.filter((s) => /\bbg-st-/.test(STATE_TONE[s]));
    expect(filled).toEqual(["RUNNING"]);
  });

  it("spells TIMED_OUT with a space in the label but not on the wire", () => {
    expect(STATE_LABEL.TIMED_OUT).toBe("TIMED OUT");
    expect(STEP_STATES as readonly string[]).toContain("TIMED_OUT");
  });
});

describe("isCancelling", () => {
  const cases: Array<{
    name: string;
    state: StepState;
    at: string | null;
    want: boolean;
  }> = [
    {
      name: "a running job the operator has asked to cancel",
      state: "RUNNING",
      at: "2026-09-12T03:24:11Z",
      want: true,
    },
    {
      name: "a queued job the operator has asked to cancel",
      state: "QUEUED",
      at: "2026-09-12T03:24:11Z",
      want: true,
    },
    {
      // Once it has settled the request is history, not a pending action.
      name: "a job that has finished cancelling",
      state: "CANCELLED",
      at: "2026-09-12T03:24:11Z",
      want: false,
    },
    {
      // Cancel raced a success and lost. The job succeeded; say so.
      name: "a job that succeeded before the cancel landed",
      state: "SUCCEEDED",
      at: "2026-09-12T03:24:11Z",
      want: false,
    },
    { name: "a job nobody cancelled", state: "RUNNING", at: null, want: false },
  ];

  it.each(cases)("$name", ({ state, at, want }) => {
    expect(isCancelling(state, at)).toBe(want);
  });
});
