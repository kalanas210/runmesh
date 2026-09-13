import type { RunMeshEvent } from "./types";

/**
 * One event, turned into the two lines the timeline prints.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS IS A PURE FUNCTION AND NOT A SWITCH INSIDE THE ROW COMPONENT.
 *
 * `attrs` is `map[string]any` on the Go side and `Record<string, unknown>`
 * here. There is no schema, the members differ per event type, and every one
 * of them is optional in the sense that a store could stop writing it without
 * breaking any test. So reading one is a narrowing exercise, and narrowing
 * done inline in JSX is narrowing nothing can assert — the failure mode is not
 * a crash but the string "undefined" printed in an operator's timeline, or
 * "[object Object]" where a reason should be.
 *
 * So the readers below are total: they answer `undefined` for a missing or
 * wrongly-typed member and the caller omits the clause rather than printing a
 * hole. That is why `describeEvent` returns `detail?: string` instead of
 * assembling a sentence that might contain gaps.
 * ---------------------------------------------------------------------------
 *
 * The switch is exhaustive over all FOURTEEN EventType values, not the ten
 * with producers. TypeScript checks that exhaustiveness at compile time, which
 * is the only mechanism that will notice when POD_CREATED finally gains a
 * writer — and `produced: false` on the four reserved types is carried through
 * so a timeline that somehow receives one can say plainly that it came from a
 * part of the runtime nothing else in this console knows about.
 */

export interface EventView {
  /** The line the row leads with, in the product's words rather than the wire's. */
  title: string;
  /** A clause assembled from `attrs`, or undefined when there is nothing to say. */
  detail?: string;
  /** Whether this event belongs to the job as a whole or to one step. */
  scope: "job" | "step";
  /**
   * False for the four EventType constants that no code path in the runtime
   * emits. A console that quietly rendered one as though it were routine would
   * be hiding the more interesting fact: something new is writing events.
   */
  produced: boolean;
}

function attrString(event: RunMeshEvent, key: string): string | undefined {
  const value = event.attrs?.[key];
  return typeof value === "string" && value !== "" ? value : undefined;
}

function attrNumber(event: RunMeshEvent, key: string): number | undefined {
  const value = event.attrs?.[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/**
 * The cancel reason, spelled the way the domain spells it.
 *
 * `step_failed` is the one worth expanding: it means the job cancelled ITSELF
 * because a step failed under fail_fast, and an operator reading "cancelled"
 * with no further explanation reasonably concludes a human did it. The step id
 * that caused it travels in the same attrs and is the first thing they would
 * otherwise go looking for.
 */
function cancelDetail(event: RunMeshEvent): string | undefined {
  const reason = attrString(event, "reason");
  if (reason === "user") return "requested by an operator";
  if (reason === "step_failed") {
    const step = attrString(event, "step_id");
    return step
      ? `fail_fast: ${step} failed, so the remaining steps were cancelled`
      : "fail_fast: a step failed, so the remaining steps were cancelled";
  }
  return reason;
}

export function describeEvent(event: RunMeshEvent): EventView {
  switch (event.type) {
    case "JOB_CREATED": {
      const steps = attrNumber(event, "steps");
      const name = attrString(event, "name");
      const parts = [
        name ? `“${name}”` : undefined,
        steps === undefined ? undefined : `${steps} ${steps === 1 ? "step" : "steps"}`,
      ].filter(Boolean);
      return {
        title: "job created",
        detail: parts.length > 0 ? parts.join(" · ") : undefined,
        scope: "job",
        produced: true,
      };
    }

    case "JOB_STARTED":
      return { title: "job started", scope: "job", produced: true };

    case "JOB_CANCEL_REQUESTED":
      return {
        title: "cancel requested",
        detail: cancelDetail(event),
        scope: "job",
        produced: true,
      };

    case "JOB_FINISHED":
      return {
        title: "job finished",
        // The rolled-up state is on the event itself rather than in attrs, and
        // the row renders it as a pill, so repeating it here would print the
        // same word twice on one line.
        detail: event.error ? `${event.error.code}: ${event.error.message}` : undefined,
        scope: "job",
        produced: true,
      };

    case "STEP_SCHEDULED":
      return {
        title: "claimed",
        // The distinction the whole waterfall is built around, stated once in
        // the timeline too: a claim is a lease, not an execution. Under the
        // Kubernetes executor the gap that opens here is the pod-pending wait.
        detail: "a worker took the lease; the tool has not started yet",
        scope: "step",
        produced: true,
      };

    case "STEP_STARTED":
      return { title: "tool started", scope: "step", produced: true };

    case "STEP_FINISHED":
      return {
        title: "step finished",
        detail: event.error ? `${event.error.code}: ${event.error.message}` : undefined,
        scope: "step",
        produced: true,
      };

    case "STEP_RETRY_SCHEDULED": {
      const at = attrString(event, "next_attempt_at");
      const failures = attrNumber(event, "failures");
      const parts = [
        at ? `not claimable until ${at}` : undefined,
        failures === undefined
          ? undefined
          : `${failures} ${failures === 1 ? "failure" : "failures"} spent`,
      ].filter(Boolean);
      return {
        title: "retry scheduled",
        detail: parts.length > 0 ? parts.join(" · ") : undefined,
        scope: "step",
        produced: true,
      };
    }

    case "STEP_RELEASED":
      return {
        title: "released",
        // A release is not a failure and spends no retry budget: the worker
        // gave the lease back, most often because the process is draining.
        // Labelling it as an error is the easiest way to make a clean shutdown
        // look like an incident.
        detail: attrString(event, "reason") ?? "the worker returned the lease without failing the step",
        scope: "step",
        produced: true,
      };

    case "STEP_LEASE_EXPIRED": {
      const owner = attrString(event, "owner");
      const failures = attrNumber(event, "failures");
      const parts = [
        owner ? `owner ${owner} stopped heartbeating` : "the lease was not renewed",
        failures === undefined ? undefined : `${failures} spent`,
      ].filter(Boolean);
      return { title: "lease expired", detail: parts.join(" · "), scope: "step", produced: true };
    }

    // The four reserved constants. They are declared in the domain so an
    // exhaustive switch can be written today, and they have no producer
    // anywhere in the repo — POD_CREATED in particular is the event that would
    // let the pending gap be split into scheduling versus image pull, and it
    // does not exist. See /runtime, which lists this among the gaps.
    case "POD_CREATED":
      return { title: "pod created", scope: "step", produced: false };
    case "POD_DELETED":
      return { title: "pod deleted", scope: "step", produced: false };
    case "TOOL_CALLED":
      return { title: "tool called", scope: "step", produced: false };
    case "STEP_OUTPUT_CHUNK":
      return { title: "output chunk", scope: "step", produced: false };
  }

  // Unreachable for a well-typed event, and deliberately not an exception. A
  // future API that emits a type this build has never heard of should produce
  // one odd-looking row, not a timeline that throws.
  const unknown = event as RunMeshEvent;
  return {
    title: String(unknown.type).toLowerCase().replace(/_/g, " "),
    detail: "this console does not recognise that event type",
    scope: unknown.step_id ? "step" : "job",
    produced: false,
  };
}

/**
 * Newest first, which is the opposite of the waterfall's order and is correct
 * for a log.
 *
 * The chart is read left to right because it is about elapsed time; a timeline
 * is read top down because it is about what just happened, and an operator
 * watching a running job wants the newest line where their eye already is.
 * Sorted on `seq` for the same reason everything else is: it is per-job,
 * gap-free, and the only total order the runtime guarantees.
 */
export function newestFirst(events: readonly RunMeshEvent[]): RunMeshEvent[] {
  return [...events].sort((a, b) => b.seq - a.seq);
}
