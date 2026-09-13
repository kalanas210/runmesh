"use client";

import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { traceSummary } from "@/lib/planner-view";
import type { PlannerTrace } from "@/lib/types";
import { KeyValue } from "@/components/ui/kv";

/**
 * The planner's working, shown in full.
 *
 * ---------------------------------------------------------------------------
 * THIS PANEL IS THE ANSWER TO "THE MODEL PRODUCED THIS".
 *
 * planner.go's own comment says the trace exists because that sentence is not
 * an acceptable answer when a plan does something surprising. Every round trip
 * is recorded with what was wrong with it, so a rejected plan is a diagnosable
 * event rather than a 400 with no history.
 *
 * A UI that rendered only the accepted plan would throw all of that away, and
 * would do it precisely on the responses where it matters: a 422 has NO
 * accepted attempt at all, so the panel a reader opens after a refusal is the
 * one that must work hardest. Hence the shape below — every attempt is a row,
 * rejected ones carry their problems, and the whole thing renders identically
 * whether it arrived with a plan or with a refusal.
 *
 * `reasoning` is populated ONLY on the accepted attempt (planner.go:98), so a
 * 422 trace has none. The section is omitted rather than rendered empty, and
 * the empty box that would otherwise appear on exactly the response someone
 * opened this for is the reason that is checked rather than assumed.
 * ---------------------------------------------------------------------------
 *
 * TOOLS OFFERED is not trivia. It is the catalogue the model was actually given
 * after the execution policy removed what this deployment refuses — a plan is
 * only explicable next to the choices available when it was made. A model that
 * never mentions http_request because it was never offered it looks like a
 * model that chose not to.
 */
export function TracePanel({ trace, className }: { trace: PlannerTrace; className?: string }) {
  const summary = traceSummary(trace);

  return (
    <section className={cn("rounded-2xl border border-line bg-ink-2 p-6", className)}>
      <h2 className="kicker">Planner trace</h2>

      <KeyValue
        className="mt-4"
        columns={2}
        rows={[
          { label: "model", value: summary.model || "—" },
          { label: "took", value: formatDuration(summary.durationMs) },
          {
            label: "attempts",
            value:
              summary.acceptedAt === null
                ? `${summary.attempts}, none accepted`
                : `${summary.attempts}, accepted on ${summary.acceptedAt}`,
          },
          {
            label: "tokens",
            // The trace's own total, not the sum of the attempts: an attempt
            // that failed before the model answered contributes to the bill
            // without contributing an output count.
            value: `${summary.totalTokens} (${summary.inputTokens} in · ${summary.outputTokens} out)`,
          },
        ]}
      />

      <section className="mt-6">
        <h3 className="kicker text-[0.5625rem]">tools offered</h3>
        <p className="mt-2 text-[0.72rem] leading-relaxed text-muted">
          {trace.tools_offered.length === 0 ? (
            <span className="text-st-retrying">
              None. Every registered tool is refused by this deployment&rsquo;s
              execution policy, so there was nothing to plan with.
            </span>
          ) : (
            <span className="tnum">{trace.tools_offered.join(" · ")}</span>
          )}
        </p>
        <p className="mt-1 text-[0.65rem] text-faint">
          The catalogue after policy removed what this deployment refuses — not
          everything the registry holds.
        </p>
      </section>

      <section className="mt-6">
        <h3 className="kicker text-[0.5625rem]">every attempt</h3>
        <ol className="mt-3 space-y-3">
          {trace.attempts.map((attempt) => (
            <li
              key={attempt.n}
              className={cn(
                "rounded-xl border px-4 py-3",
                attempt.accepted
                  ? "border-st-succeeded/40 bg-st-succeeded/[0.05]"
                  : "border-line bg-ink",
              )}
            >
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <span className="tnum text-[0.78rem] text-bone">attempt {attempt.n}</span>
                <span
                  className={cn(
                    "rounded-full border px-2 py-0 font-mono text-[0.55rem] uppercase tracking-[0.14em]",
                    attempt.accepted
                      ? "border-st-succeeded/50 text-st-succeeded"
                      : "border-line-2 text-muted",
                  )}
                >
                  {attempt.accepted ? "accepted" : "rejected"}
                </span>
                <span className="tnum ml-auto text-[0.62rem] text-faint">
                  {attempt.input_tokens} in · {attempt.output_tokens} out
                </span>
              </div>

              {attempt.error && (
                // Not the plan's content: the API refused, the response was
                // truncated, the JSON did not parse. Kept separate from the
                // validation problems below because they are fixed by different
                // people — this one by whoever owns the key or the quota.
                <p className="mt-2 font-mono text-[0.7rem] leading-snug text-[#ff6b6b]">
                  {attempt.error}
                </p>
              )}

              {attempt.problems && attempt.problems.length > 0 && (
                <ul className="mt-2 space-y-1">
                  {attempt.problems.map((problem, index) => (
                    <li key={`${problem.field}-${index}`} className="text-[0.7rem] leading-snug">
                      <span className="tnum text-st-retrying">{problem.field}</span>
                      <span className="ml-2 text-muted">{problem.issue}</span>
                    </li>
                  ))}
                </ul>
              )}

              {attempt.accepted && !attempt.error && (
                <p className="mt-2 text-[0.68rem] text-faint">
                  Validated against this deployment&rsquo;s limits and tool
                  catalogue. Submitting it runs it through the same gate again.
                </p>
              )}
            </li>
          ))}
        </ol>
      </section>

      {summary.hasReasoning && (
        <section className="mt-6">
          <h3 className="kicker text-[0.5625rem]">the model&rsquo;s reasoning</h3>
          {/* Rendered as plain text in a paragraph, not as Markdown. It is model
              output, and this console renders no model output as markup. */}
          <p className="mt-2 whitespace-pre-wrap text-[0.75rem] leading-relaxed text-muted">
            {trace.reasoning}
          </p>
        </section>
      )}
    </section>
  );
}
