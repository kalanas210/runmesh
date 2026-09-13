import { describe, expect, it } from "vitest";
import { describeEvent, newestFirst } from "./event-view";
import { EVENT_TYPES, type RunMeshEvent } from "./types";

function event(type: RunMeshEvent["type"], attrs?: Record<string, unknown>): RunMeshEvent {
  return {
    global_seq: 10,
    seq: 3,
    job_id: "job_1",
    type,
    at: "2026-09-12T03:00:00Z",
    attrs,
  };
}

describe("describeEvent", () => {
  it("covers every declared event type without falling through", () => {
    // The switch is exhaustive over all fourteen constants, not the ten with
    // producers, so that TypeScript notices when POD_CREATED finally gains a
    // writer. This asserts the runtime half of the same claim.
    for (const type of EVENT_TYPES) {
      const view = describeEvent(event(type));
      expect(view.title).not.toBe("");
      expect(view.title).not.toContain("undefined");
    }
  });

  it("marks the four reserved types as not produced", () => {
    for (const type of ["POD_CREATED", "POD_DELETED", "TOOL_CALLED", "STEP_OUTPUT_CHUNK"] as const) {
      expect(describeEvent(event(type)).produced).toBe(false);
    }
    expect(describeEvent(event("STEP_STARTED")).produced).toBe(true);
  });

  it("expands a fail_fast cancel, which is otherwise read as a human doing it", () => {
    const view = describeEvent(
      event("JOB_CANCEL_REQUESTED", { reason: "step_failed", step_id: "fetch" }),
    );
    expect(view.detail).toContain("fetch");
    expect(view.detail).toContain("fail_fast");
  });

  it("names the operator for a user cancel", () => {
    expect(describeEvent(event("JOB_CANCEL_REQUESTED", { reason: "user" })).detail).toBe(
      "requested by an operator",
    );
  });

  it("omits a clause whose attr is missing rather than printing undefined", () => {
    // attrs is map[string]any with no schema. The failure mode of reading one
    // carelessly is not a crash, it is the word "undefined" in an operator's
    // timeline.
    const view = describeEvent(event("JOB_CREATED"));
    expect(view.detail).toBeUndefined();
  });

  it("omits a clause whose attr is present with the wrong type", () => {
    const view = describeEvent(event("JOB_CREATED", { steps: "four", name: 7 }));
    expect(view.detail).toBeUndefined();
  });

  it("reads the retry backoff and the failure budget", () => {
    const view = describeEvent(
      event("STEP_RETRY_SCHEDULED", { next_attempt_at: "2026-09-12T03:00:02Z", failures: 1 }),
    );
    expect(view.detail).toContain("2026-09-12T03:00:02Z");
    expect(view.detail).toContain("1 failure spent");
  });

  it("names the owner that stopped heartbeating on a lease expiry", () => {
    const view = describeEvent(event("STEP_LEASE_EXPIRED", { owner: "worker-2", failures: 2 }));
    expect(view.detail).toContain("worker-2");
  });

  it("does not describe a release as a failure", () => {
    // A release spends no retry budget and is the normal shape of a clean
    // drain. Labelling it as an error makes a graceful shutdown look like an
    // incident.
    const view = describeEvent(event("STEP_RELEASED", { reason: "shutdown" }));
    expect(view.detail).toBe("shutdown");
    expect(view.title).not.toContain("fail");
  });

  it("keeps the claim and the start as two different facts", () => {
    // The pod-pending gap lives between these two events and is the single most
    // valuable interval on the waterfall. A timeline that called both "started"
    // would erase it in the other rendering of the same data.
    expect(describeEvent(event("STEP_SCHEDULED")).title).toBe("claimed");
    expect(describeEvent(event("STEP_STARTED")).title).toBe("tool started");
  });

  it("renders an unknown type as one odd row rather than throwing", () => {
    const rogue = { ...event("STEP_STARTED"), type: "STEP_TELEPORTED" } as unknown as RunMeshEvent;
    const view = describeEvent(rogue);
    expect(view.title).toBe("step teleported");
    expect(view.produced).toBe(false);
  });
});

describe("newestFirst", () => {
  it("orders on the per-job seq, descending", () => {
    const events = [event("STEP_STARTED"), { ...event("STEP_FINISHED"), seq: 9 }];
    expect(newestFirst(events).map((e) => e.seq)).toEqual([9, 3]);
  });

  it("does not mutate its input", () => {
    const events = [{ ...event("STEP_STARTED"), seq: 1 }, { ...event("STEP_STARTED"), seq: 2 }];
    newestFirst(events);
    expect(events.map((e) => e.seq)).toEqual([1, 2]);
  });
});
