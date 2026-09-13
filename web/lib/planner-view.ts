import type { Plan, PlannerTrace } from "./types";

/**
 * The planner screen's pure half: what a trace adds up to, and how a plan
 * moves between the reviewer's textarea and the wire.
 *
 * ---------------------------------------------------------------------------
 * WHY THE PLAN IS EDITABLE TEXT AT ALL.
 *
 * `POST /api/v1/plans` exists so that a plan can be SEEN before anything runs
 * — plans.go says so in as many words: propose, review, submit. A review
 * surface that renders the plan as a read-only summary and then posts the
 * object it was given back is not a review, it is a confirmation dialog. The
 * reviewer's one real power is to change a parameter, drop a step or fix a
 * dependency before it executes, and that requires the thing they are looking
 * at to be the thing that gets submitted.
 *
 * So the accepted plan lands in a textarea as canonical JSON and is posted
 * from there. The cost is that a reviewer can produce invalid JSON, which is
 * why `parsePlanDraft` exists and why the screen re-checks before it will let
 * the submit happen. The alternative — a generated form with a field per step
 * — cannot represent `params`, which is untyped raw JSON per tool.
 * ---------------------------------------------------------------------------
 */

export interface TraceSummary {
  model: string;
  /** Round trips to the model. */
  attempts: number;
  /** The 1-based `n` of the accepted attempt, or null when none was accepted. */
  acceptedAt: number | null;
  rejected: number;
  inputTokens: number;
  outputTokens: number;
  /** The trace's own total, which is authoritative — see the note below. */
  totalTokens: number;
  durationMs: number;
  toolsOffered: number;
  /**
   * Populated ONLY on the accepted attempt (planner.go:98), so a 422 trace has
   * none. A panel that assumed otherwise would render an empty box on exactly
   * the response a reader opened it for.
   */
  hasReasoning: boolean;
}

export function traceSummary(trace: PlannerTrace): TraceSummary {
  let inputTokens = 0;
  let outputTokens = 0;
  let acceptedAt: number | null = null;
  let rejected = 0;

  for (const attempt of trace.attempts) {
    inputTokens += attempt.input_tokens;
    outputTokens += attempt.output_tokens;
    if (attempt.accepted) acceptedAt = attempt.n;
    else rejected += 1;
  }

  return {
    model: trace.model,
    attempts: trace.attempts.length,
    acceptedAt,
    rejected,
    inputTokens,
    outputTokens,
    // NOT inputTokens + outputTokens. The trace carries its own total and the
    // two can legitimately differ: an attempt that failed before the model
    // answered contributes to the bill without contributing an output count,
    // and the heuristic planner reports zero for both halves while still
    // recording a total of zero. Recomputing it here would silently replace
    // the runtime's number with the console's arithmetic.
    totalTokens: trace.total_tokens,
    durationMs: trace.duration_ms,
    toolsOffered: trace.tools_offered.length,
    hasReasoning: !!trace.reasoning && trace.reasoning.trim() !== "",
  };
}

/**
 * A plan as the editor shows it: two-space JSON, keys in the order the Go
 * struct declares them.
 *
 * The key order is not cosmetic. `JSON.stringify` emits insertion order, and
 * the object arriving from the API already has the Go field order, so passing
 * it straight through would usually be right — but a plan that has been
 * through `JSON.parse` after a reviewer edited it has whatever order they
 * typed. Rebuilding it here means the diff a reviewer sees between the
 * proposed plan and their edit is their edit, and not a reshuffle.
 *
 * Optional members are omitted when unset rather than written as null:
 * `on_step_failure` is `omitempty` on the Go side and decodeJSON uses
 * DisallowUnknownFields, so an explicit null would be decoded into the zero
 * value rather than the default — a plan that quietly becomes fail_fast when
 * the planner asked for continue_on_failure.
 */
export function formatPlan(plan: Plan): string {
  const out: Record<string, unknown> = { name: plan.name };
  if (plan.priority !== undefined && plan.priority !== 0) out.priority = plan.priority;
  if (plan.on_step_failure) out.on_step_failure = plan.on_step_failure;

  out.steps = plan.steps.map((step) => {
    const s: Record<string, unknown> = { id: step.id, tool: step.tool };
    if (step.params !== undefined) s.params = step.params;
    if (step.depends_on && step.depends_on.length > 0) s.depends_on = step.depends_on;
    if (step.timeout_seconds) s.timeout_seconds = step.timeout_seconds;
    if (step.max_attempts) s.max_attempts = step.max_attempts;
    return s;
  });

  return `${JSON.stringify(out, null, 2)}\n`;
}

export type PlanDraft =
  | { ok: true; plan: Plan }
  | { ok: false; error: string };

/**
 * Read the editor's text back into a plan, refusing anything that is not one.
 *
 * The shape checks here deliberately stop at the top level. `Plan.Validate` on
 * the Go side collects EVERY problem — unknown tools, unknown dependencies,
 * cycles, over-long timeouts — and reports them per field, and duplicating any
 * part of that here would produce a second validator that disagrees with the
 * first one the moment a limit is reconfigured. What this checks is only what
 * the server cannot usefully report: a body that is not JSON at all, or is not
 * a plan-shaped object, gets a 400 whose message is about decoding rather than
 * about the plan. Everything else is the server's to judge, and its answer is
 * rendered per field by the problem list.
 */
export function parsePlanDraft(source: string): PlanDraft {
  const text = source.trim();
  if (text === "") return { ok: false, error: "The plan is empty." };

  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (cause) {
    return {
      ok: false,
      // The browser's own message names the character offset, which is the
      // most useful thing anyone can say about malformed JSON.
      error: cause instanceof Error ? cause.message : "That is not valid JSON.",
    };
  }

  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, error: "A plan is a JSON object, not an array or a bare value." };
  }

  const record = parsed as Record<string, unknown>;
  if (!Array.isArray(record.steps)) {
    return { ok: false, error: 'A plan needs a "steps" array.' };
  }

  return { ok: true, plan: record as unknown as Plan };
}

/**
 * The plan the editor opens with when nobody has planned anything yet.
 *
 * echo → report_generate, because those two are registered in every
 * deployment (builtin.go registers echo and sleep unconditionally, report.go
 * adds report_generate) and because the pair demonstrates the one thing a
 * single-step plan cannot: that a dependency's output flows along the edge.
 * A first submission that succeeds is worth more than a first submission that
 * shows off — the failure path is one bad tool name away and is reachable on
 * purpose, whereas a starter plan that fails teaches a reader nothing about
 * whether their console works.
 */
export const STARTER_PLAN = `{
  "name": "hello runmesh",
  "steps": [
    {
      "id": "greet",
      "tool": "echo",
      "params": { "message": "hello from the console" }
    },
    {
      "id": "report",
      "tool": "report_generate",
      "depends_on": ["greet"],
      "params": { "title": "First run", "summary": "Submitted from the RunMesh console." }
    }
  ]
}
`;
