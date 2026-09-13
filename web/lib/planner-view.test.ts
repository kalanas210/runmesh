import { describe, expect, it } from "vitest";
import { STARTER_PLAN, formatPlan, parsePlanDraft, traceSummary } from "./planner-view";
import type { PlannerTrace } from "./types";

function trace(partial: Partial<PlannerTrace> = {}): PlannerTrace {
  return {
    model: "gemini-2.0-flash",
    attempts: [],
    tools_offered: ["echo", "sleep"],
    duration_ms: 1200,
    total_tokens: 0,
    ...partial,
  };
}

describe("traceSummary", () => {
  it("reports the trace's own total rather than recomputing it from the attempts", () => {
    // The two can legitimately differ: an attempt that failed before the model
    // answered contributes to the bill without contributing an output count.
    // Recomputing would silently replace the runtime's number with ours.
    const summary = traceSummary(
      trace({
        total_tokens: 999,
        attempts: [{ n: 1, input_tokens: 10, output_tokens: 20, accepted: true }],
      }),
    );
    expect(summary.totalTokens).toBe(999);
    expect(summary.inputTokens).toBe(10);
    expect(summary.outputTokens).toBe(20);
  });

  it("finds the accepted attempt and counts the rejected ones", () => {
    const summary = traceSummary(
      trace({
        attempts: [
          { n: 1, input_tokens: 5, output_tokens: 5, accepted: false, problems: [] },
          { n: 2, input_tokens: 5, output_tokens: 5, accepted: false, error: "truncated" },
          { n: 3, input_tokens: 5, output_tokens: 5, accepted: true },
        ],
      }),
    );
    expect(summary.acceptedAt).toBe(3);
    expect(summary.rejected).toBe(2);
    expect(summary.attempts).toBe(3);
  });

  it("reports no accepted attempt for a 422 trace", () => {
    // The failure case is the one the panel exists for, and it is the one where
    // every field a naive implementation depends on is absent.
    const summary = traceSummary(
      trace({ attempts: [{ n: 1, input_tokens: 5, output_tokens: 5, accepted: false }] }),
    );
    expect(summary.acceptedAt).toBeNull();
    expect(summary.hasReasoning).toBe(false);
  });

  it("does not claim reasoning for an empty string", () => {
    expect(traceSummary(trace({ reasoning: "   " })).hasReasoning).toBe(false);
    expect(traceSummary(trace({ reasoning: "because" })).hasReasoning).toBe(true);
  });
});

describe("formatPlan", () => {
  it("omits an unset on_step_failure rather than writing null", () => {
    // decodeJSON uses DisallowUnknownFields and the field is omitempty, so an
    // explicit null decodes to the zero value — a plan that quietly becomes
    // fail_fast when the planner asked for something else.
    const text = formatPlan({ name: "p", steps: [{ id: "a", tool: "echo" }] });
    expect(text).not.toContain("on_step_failure");
    expect(text).not.toContain("null");
  });

  it("keeps params verbatim, since they are untyped per tool", () => {
    const text = formatPlan({
      name: "p",
      steps: [{ id: "a", tool: "echo", params: { nested: { deep: [1, 2] } } }],
    });
    expect(JSON.parse(text).steps[0].params).toEqual({ nested: { deep: [1, 2] } });
  });

  it("round-trips through the draft parser", () => {
    const plan = {
      name: "p",
      on_step_failure: "continue_on_failure" as const,
      steps: [
        { id: "a", tool: "echo", params: { m: 1 } },
        { id: "b", tool: "report_generate", depends_on: ["a"], max_attempts: 2 },
      ],
    };
    const draft = parsePlanDraft(formatPlan(plan));
    expect(draft.ok).toBe(true);
    if (draft.ok) expect(draft.plan).toEqual(plan);
  });
});

describe("parsePlanDraft", () => {
  it("rejects an empty editor with a sentence rather than a parse error", () => {
    const draft = parsePlanDraft("   \n  ");
    expect(draft.ok).toBe(false);
    if (!draft.ok) expect(draft.error).toBe("The plan is empty.");
  });

  it("reports the JSON parser's own message, which names the offset", () => {
    const draft = parsePlanDraft('{"name": "p",}');
    expect(draft.ok).toBe(false);
    if (!draft.ok) expect(draft.error.length).toBeGreaterThan(0);
  });

  it("refuses an array, which is the shape a model most often produces by mistake", () => {
    const draft = parsePlanDraft('[{"id":"a","tool":"echo"}]');
    expect(draft.ok).toBe(false);
  });

  it("refuses an object with no steps array", () => {
    expect(parsePlanDraft('{"name":"p"}').ok).toBe(false);
    expect(parsePlanDraft('{"name":"p","steps":{}}').ok).toBe(false);
  });

  it("does not attempt the validation the server does properly", () => {
    // Plan.Validate collects every problem per field. A second validator here
    // would disagree with it the moment a limit is reconfigured, so an unknown
    // tool and a cyclic dependency both parse fine and are the server's to
    // refuse.
    const draft = parsePlanDraft('{"name":"p","steps":[{"id":"a","tool":"nope","depends_on":["a"]}]}');
    expect(draft.ok).toBe(true);
  });

  it("accepts the starter plan, which is the first thing anyone submits", () => {
    const draft = parsePlanDraft(STARTER_PLAN);
    expect(draft.ok).toBe(true);
    if (draft.ok) {
      expect(draft.plan.steps.map((s) => s.tool)).toEqual(["echo", "report_generate"]);
      expect(draft.plan.steps[1].depends_on).toEqual(["greet"]);
    }
  });
});
