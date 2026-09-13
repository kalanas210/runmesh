"use client";

import { cn } from "@/lib/cn";
import { formatDuration, formatInstant } from "@/lib/format";
import { STATE_LABEL } from "@/lib/state";
import type { JobResponse } from "@/lib/types";
import { spanTotals, type Lane, type Segment } from "@/lib/waterfall";

/**
 * The chart, as a table. Not a fallback — the same data, in the other form.
 *
 * This is the accessible rendering of the waterfall and it is rendered ALWAYS,
 * `sr-only` beside the chart and visible when the reader asks for it. That is
 * the whole approach to accessibility here, and it is deliberate: a chart made
 * of six hundred absolutely-positioned coloured rectangles cannot be made
 * comprehensible to a screen reader by adding attributes to the rectangles. The
 * bars carry meaning through position and length, and neither survives being
 * read aloud. What does survive is a table, so the table is real markup with a
 * caption, real headers and real scopes, and the chart is the decorative twin
 * of it rather than the other way round.
 *
 * It is also the answer for a sighted reader who cannot separate two of the
 * hues, and for anyone who wants to copy a timestamp out of the screen — which
 * a rectangle does not allow and a `<td>` does.
 *
 * Absolute UTC in every instant column. An operator takes these numbers to a
 * log line or a pod event, and both of those are in UTC.
 */
export function WaterfallTable({
  job,
  lanes,
  segmentsByLane,
  className,
}: {
  job: JobResponse;
  lanes: readonly Lane[];
  /** The same segments the chart drew, keyed by step id, so the two renderings
   *  cannot report different numbers for the same lane. */
  segmentsByLane: ReadonlyMap<string, readonly Segment[]>;
  className?: string;
}) {
  return (
    <table className={cn("w-full border-collapse text-left", className)}>
      <caption className="sr-only">
        Every recorded attempt of every step in job {job.name}, in plan order. One
        row per attempt. Queued times are inferred from the job&apos;s creation and
        its dependencies&apos; completion, because the runtime records no
        per-step eligibility timestamp.
      </caption>
      <thead>
        <tr className="border-b border-line">
          {[
            "step",
            "tool",
            "depth",
            "attempt",
            "state",
            "queued",
            "pending",
            "ran",
            "started (UTC)",
            "ended (UTC)",
            "error",
          ].map((heading) => (
            <th
              key={heading}
              scope="col"
              className="kicker whitespace-nowrap px-2 py-1.5 text-[0.5625rem]"
            >
              {heading}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {lanes.map((lane) => {
          const totals = spanTotals(segmentsByLane.get(lane.stepId) ?? []);

          // A lane with no attempts is not an empty row to be skipped: it is a
          // step that has not been claimed yet, or one whose attempts were
          // evicted from the ring, and both are facts a reader is looking for.
          if (lane.attempts.length === 0) {
            return (
              <tr key={lane.stepId} className="border-b border-line/60">
                <th scope="row" className="tnum px-2 py-1.5 text-[0.7rem] font-normal text-bone-2">
                  {lane.stepId}
                </th>
                <td className="px-2 py-1.5 text-[0.65rem] text-faint">{lane.tool}</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">{lane.depth}</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">—</td>
                <td className="px-2 py-1.5 text-[0.65rem]">{STATE_LABEL[lane.state]}</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">
                  {totals.queuedMs > 0 ? `${formatDuration(totals.queuedMs)} (inferred)` : "—"}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">—</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">—</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">—</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">—</td>
                <td className="px-2 py-1.5 text-[0.65rem] text-faint">
                  {lane.state === "QUEUED" && lane.blockedBy.length > 0
                    ? `blocked by ${lane.blockedBy.join(", ")}`
                    : "no attempts recorded"}
                </td>
              </tr>
            );
          }

          return lane.attempts.map((attempt, index) => {
            const pendingMs =
              attempt.scheduledAt !== undefined && attempt.startedAt !== undefined
                ? attempt.startedAt - attempt.scheduledAt
                : undefined;

            return (
              <tr key={`${lane.stepId}:${attempt.n}:${attempt.seq}`} className="border-b border-line/60">
                <th scope="row" className="tnum px-2 py-1.5 text-[0.7rem] font-normal text-bone-2">
                  {lane.stepId}
                </th>
                <td className="px-2 py-1.5 text-[0.65rem] text-faint">{lane.tool}</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">{lane.depth}</td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">
                  {attempt.n} of {lane.maxAttempts}
                </td>
                <td className="px-2 py-1.5 text-[0.65rem]">
                  {/* The word, not the colour. This column is the reason the
                      table exists. */}
                  {attempt.open ? "RUNNING, still open" : STATE_LABEL[attempt.state]}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">
                  {/* The queued total belongs to the LANE, not to an attempt —
                      it is the sum of every gap between attempts — so it is
                      printed once, on the first row, rather than repeated in a
                      way that would invite a reader to add it up. */}
                  {index === 0 && totals.queuedMs > 0
                    ? `${formatDuration(totals.queuedMs)}${totals.inferred ? " (inferred)" : ""}`
                    : "—"}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">
                  {pendingMs === undefined ? "—" : formatDuration(pendingMs)}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-muted">
                  {/* duration_ms as the runtime measured it, never recomputed
                      from two timestamps, so this column agrees with every
                      other place the API's own number is printed. */}
                  {attempt.durationMs !== undefined
                    ? formatDuration(attempt.durationMs)
                    : attempt.neverRan
                      ? "never ran"
                      : "—"}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">
                  {attempt.startedAt !== undefined
                    ? formatInstant(new Date(attempt.startedAt).toISOString())
                    : "—"}
                </td>
                <td className="tnum px-2 py-1.5 text-[0.65rem] text-faint">
                  {attempt.open
                    ? "still open"
                    : attempt.endedAt !== undefined
                      ? formatInstant(new Date(attempt.endedAt).toISOString())
                      : "—"}
                </td>
                <td className="px-2 py-1.5 text-[0.65rem] text-muted">
                  {attempt.error
                    ? `${attempt.error.code}${attempt.error.retryable ? " (retryable)" : ""}: ${attempt.error.message}`
                    : attempt.expiredOwner !== undefined
                      ? `lease expired, owner ${attempt.expiredOwner}`
                      : attempt.releasedReason !== undefined
                        ? `released: ${attempt.releasedReason}`
                        : "—"}
                </td>
              </tr>
            );
          });
        })}
      </tbody>
    </table>
  );
}
