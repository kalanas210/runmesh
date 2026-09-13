"use client";

import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { isCancelling } from "@/lib/state";
import type { JobResponse } from "@/lib/types";
import { StatePill } from "@/components/ui/state-pill";

/**
 * The steps as a table, in plan order.
 *
 * ---------------------------------------------------------------------------
 * IT IS NEVER SORTED, AND THE ORDER CANNOT BE RESTORED IF IT IS.
 *
 * `ordinal` is not on the wire. The order of `job.steps` IS the plan's order,
 * and it is the only record of it that reaches the client — so a column header
 * that sorted by duration would destroy information no request can fetch back.
 * That is why this table has no sort controls: not because sorting is hard, but
 * because the array's order is data.
 * ---------------------------------------------------------------------------
 *
 * This is the same steps the waterfall draws as lanes, and clicking a row
 * selects the lane — one selection, two renderings. The table exists beside the
 * chart rather than instead of it because the two answer different questions: a
 * chart is for "where did the time go" and a table is for "what is the state of
 * step fetch_3", which is the question somebody arrives with when they were
 * told a step id over a call.
 *
 * `duration_ms` is `ended_at - started_at`, so it is TOOL EXECUTION time and
 * excludes the claimed-but-not-started gap. The header says so, because a
 * reader comparing this column against the waterfall's bar widths will
 * otherwise conclude one of them is wrong.
 */
export function StepTable({
  job,
  selected,
  onSelect,
  className,
}: {
  job: JobResponse;
  selected: string | null;
  onSelect: (stepId: string) => void;
  className?: string;
}) {
  const th = "px-4 py-2.5 text-left font-normal kicker text-[0.5625rem] whitespace-nowrap";
  const td = "px-4 py-2.5 align-middle";

  return (
    <div className={cn("overflow-x-auto rounded-2xl border border-line bg-ink-2", className)}>
      <table className="w-full border-collapse text-[0.78rem]">
        <caption className="px-4 pt-3 text-left text-[0.65rem] text-faint">
          In plan order. Duration is tool execution only — it excludes the time a
          step spent claimed but not yet started.
        </caption>
        <thead>
          <tr className="border-b border-line text-muted">
            <th scope="col" className={th}>
              step
            </th>
            <th scope="col" className={th}>
              tool
            </th>
            <th scope="col" className={th}>
              state
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              attempt
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              duration
            </th>
            <th scope="col" className={th}>
              waiting on
            </th>
          </tr>
        </thead>
        <tbody>
          {job.steps.map((step) => (
            <tr
              key={step.id}
              onClick={() => onSelect(step.id)}
              className={cn(
                "cursor-pointer border-b border-line/60 transition-colors last:border-b-0 hover:bg-bone/[0.03]",
                selected === step.id && "bg-accent-soft",
              )}
              style={{ transitionDuration: "var(--dur-hover)" }}
            >
              <td className={cn(td, "tnum text-bone")}>{step.id}</td>
              <td className={cn(td, "text-muted")}>{step.tool}</td>
              <td className={td}>
                <StatePill
                  state={step.state}
                  size="sm"
                  cancelling={isCancelling(step.state, job.cancel_requested_at)}
                />
              </td>
              <td className={cn(td, "tnum text-right text-muted")}>
                {step.attempt}
                <span className="text-faint">/{step.max_attempts}</span>
                {/* Only when it differs. attempt increments on every claim and
                    failures only on a real failure, so they agree on the happy
                    path and printing both would be noise there. */}
                {step.failures > 0 && (
                  <span className="ml-1.5 text-st-retrying">{step.failures} failed</span>
                )}
              </td>
              <td className={cn(td, "tnum text-right text-muted")}>
                {step.duration_ms === null ? "—" : formatDuration(step.duration_ms)}
              </td>
              <td className={cn(td, "tnum max-w-[14rem] truncate text-[0.7rem]")}>
                {step.blocked_by.length > 0 ? (
                  // Derived per request by Job.BlockedBy and stored nowhere, so
                  // it is read straight off this snapshot every render.
                  <span className="text-st-queued">{step.blocked_by.join(", ")}</span>
                ) : (
                  <span className="text-faint">—</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
