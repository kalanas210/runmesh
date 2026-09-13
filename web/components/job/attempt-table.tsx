"use client";

import { cn } from "@/lib/cn";
import { formatDuration, formatTimeOfDay } from "@/lib/format";
import type { Attempt } from "@/lib/waterfall";
import { StatePill } from "@/components/ui/state-pill";

/**
 * Every execution of one step, as rows.
 *
 * ---------------------------------------------------------------------------
 * THIS TABLE CANNOT BE BUILT FROM THE JOB SNAPSHOT, AND THAT IS THE WHOLE
 * REASON IT EXISTS.
 *
 * `stepResponse` carries ONE set of timestamps. `claimOneLocked` overwrites
 * `scheduled_at`, nulls `started_at` and `ended_at` and increments `attempt` on
 * every claim; Release and ExpireLeases null them outright. So the row an
 * operator fetches describes the CURRENT attempt and nothing else, and a step
 * that failed three times shows one clean set of times with no trace of the
 * other two.
 *
 * These rows come from the event stream instead, folded by `buildLanes`. That
 * is also why `attempt` and `failures` are different columns and why they
 * legitimately disagree: `attempt` is monotonic and increments on every claim,
 * including a claim after a release or a lease expiry, which spends no retry
 * budget. A step showing attempt 4 with 1 failure is not a bug, and an
 * operator who reads this table needs both numbers to tell those cases apart.
 * ---------------------------------------------------------------------------
 *
 * `pending` — claimed to started — is its own column and is never folded into
 * `ran`. Under the Kubernetes executor it is the pod-pending wait and is
 * routinely seconds, and `duration_ms` on the wire already excludes it, so a
 * table that merged them would disagree with the number printed beside it.
 */
export function AttemptTable({
  attempts,
  maxAttempts,
  selected,
  onSelect,
}: {
  attempts: readonly Attempt[];
  maxAttempts: number;
  selected?: number | null;
  onSelect?: (attempt: number) => void;
}) {
  if (attempts.length === 0) {
    return (
      <p className="text-[0.75rem] leading-relaxed text-faint">
        No attempt survives in the event history for this step. Either it has
        not been claimed yet, or its events were evicted from the in-memory
        ring — the banner above the timeline says which.
      </p>
    );
  }

  const th = "px-2.5 py-1.5 text-left font-normal kicker text-[0.5rem] whitespace-nowrap";
  const td = "px-2.5 py-2 align-middle whitespace-nowrap";

  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-[0.72rem]">
        <caption className="sr-only">
          Every recorded attempt of this step, with the time it spent claimed but
          not started, and the time the tool was executing.
        </caption>
        <thead>
          <tr className="border-b border-line text-muted">
            <th scope="col" className={th}>
              #
            </th>
            <th scope="col" className={th}>
              state
            </th>
            <th scope="col" className={th}>
              claimed
            </th>
            <th scope="col" className={th}>
              started
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              pending
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              ran
            </th>
          </tr>
        </thead>
        <tbody>
          {attempts.map((attempt) => {
            const pending =
              attempt.scheduledAt !== undefined && attempt.startedAt !== undefined
                ? attempt.startedAt - attempt.scheduledAt
                : null;

            return (
              <tr
                // Two attempts can share `n` — a cancel can close a step that
                // was already closed as RETRYING — so the opening event's seq is
                // the only key that cannot collide.
                key={attempt.seq}
                onClick={onSelect ? () => onSelect(attempt.n) : undefined}
                className={cn(
                  "border-b border-line/60 last:border-b-0",
                  onSelect && "cursor-pointer hover:bg-bone/[0.03]",
                  selected === attempt.n && "bg-accent-soft",
                )}
              >
                <td className={cn(td, "tnum text-muted")}>
                  {attempt.n}
                  <span className="text-faint">/{maxAttempts}</span>
                </td>
                <td className={td}>
                  <StatePill state={attempt.state} size="sm" />
                </td>
                <td className={cn(td, "tnum text-muted")}>
                  {attempt.scheduledAt === undefined
                    ? "—"
                    : formatTimeOfDay(new Date(attempt.scheduledAt).toISOString())}
                </td>
                <td className={cn(td, "tnum text-muted")}>
                  {attempt.startedAt === undefined
                    ? "—"
                    : formatTimeOfDay(new Date(attempt.startedAt).toISOString())}
                </td>
                <td className={cn(td, "tnum text-right text-st-scheduled")}>
                  {pending === null ? "—" : formatDuration(pending)}
                </td>
                <td className={cn(td, "tnum text-right text-bone-2")}>
                  {/* As REPORTED by the closing event, not recomputed from two
                      timestamps, so it agrees with the number the API prints
                      elsewhere on this screen. */}
                  {attempt.durationMs === undefined ? "—" : formatDuration(attempt.durationMs)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      {/* The facts that belong to an attempt but not to a column: they appear on
          a minority of rows and a column for each would be six empty ones. */}
      <ul className="mt-2 space-y-1">
        {attempts.map((attempt) => {
          const notes: string[] = [];
          if (attempt.neverRan) {
            notes.push("never ran — closed before it was ever claimed");
          }
          if (attempt.backoffUntil !== undefined) {
            notes.push(
              `backoff until ${formatTimeOfDay(new Date(attempt.backoffUntil).toISOString())}`,
            );
          }
          if (attempt.expiredOwner) {
            notes.push(`lease expired; owner ${attempt.expiredOwner} stopped heartbeating`);
          }
          if (attempt.releasedReason) {
            notes.push(`released: ${attempt.releasedReason}`);
          }
          if (attempt.failures !== undefined) {
            notes.push(`${attempt.failures} of the retry budget spent`);
          }
          if (attempt.clamped) {
            notes.push("too short to draw to scale — the chart clamps this bar to 3px");
          }
          if (notes.length === 0) return null;

          return (
            <li key={`note-${attempt.seq}`} className="text-[0.65rem] leading-snug text-faint">
              <span className="tnum text-muted">#{attempt.n}</span> {notes.join(" · ")}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
