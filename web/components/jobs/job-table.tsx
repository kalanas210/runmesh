"use client";

import Link from "next/link";
import { cn } from "@/lib/cn";
import { formatDuration, formatInstant } from "@/lib/format";
import { isCancelling } from "@/lib/state";
import type { JobResponse } from "@/lib/types";
import { StatePill } from "@/components/ui/state-pill";

/**
 * The job list, as a real table.
 *
 * A `<table>` and not a grid of divs, because it is tabular data and because
 * the semantics are the accessibility: a screen reader announces the column
 * header with each cell, so "state, failed" instead of "failed" floating
 * between an id and a number. Nothing else gives that for free.
 *
 * ONLY THE ID CELL IS A LINK. Wrapping the whole row in an anchor is the usual
 * shortcut and it produces a link whose accessible name is the entire row read
 * aloud — id, name, state, step count, two timestamps — which is useless in a
 * links list and exhausting in a row-by-row read. A single link with the id as
 * its text is the shortest name that identifies the destination. The rest of
 * the row highlights with it on hover, which is the affordance people actually
 * want from a "clickable row".
 *
 * The duration column is `duration_ms`, which is `ended_at - started_at`. For a
 * job still running it is null, and the cell says so rather than counting up:
 * a ticking number in a table that repaints every five seconds is motion that
 * signals nothing, and this product's rule is that motion signals state change.
 */
export function JobTable({
  jobs,
  stale = false,
  className,
}: {
  jobs: readonly JobResponse[];
  /** The rows are the previous page's answer while a new one is in flight. */
  stale?: boolean;
  className?: string;
}) {
  const th = "px-4 py-2.5 text-left font-normal kicker text-[0.5625rem] whitespace-nowrap";
  const td = "px-4 py-3 align-middle";

  return (
    <div
      className={cn(
        "overflow-x-auto rounded-2xl border border-line bg-ink-2 transition-opacity",
        // Dimmed rather than replaced. These rows ARE still the last answer the
        // runtime gave, and blanking them to show a loader for a request that
        // usually takes single-digit milliseconds is a flash of nothing.
        stale && "opacity-60",
        className,
      )}
      style={{ transitionDuration: "var(--dur-state)" }}
    >
      <table className="w-full border-collapse text-[0.82rem]">
        <thead>
          <tr className="border-b border-line text-muted">
            <th scope="col" className={th}>
              id
            </th>
            <th scope="col" className={th}>
              name
            </th>
            <th scope="col" className={th}>
              state
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              steps
            </th>
            <th scope="col" className={th}>
              created
            </th>
            <th scope="col" className={cn(th, "text-right")}>
              duration
            </th>
          </tr>
        </thead>
        <tbody>
          {jobs.map((job) => (
            <tr
              key={job.id}
              className="group border-b border-line/60 transition-colors last:border-b-0 hover:bg-bone/[0.03]"
              style={{ transitionDuration: "var(--dur-hover)" }}
            >
              <td className={cn(td, "tnum")}>
                <Link href={`/jobs/${job.id}`} className="ulink text-bone">
                  {job.id}
                </Link>
              </td>
              <td className={cn(td, "max-w-[18rem] truncate text-bone-2")} title={job.name}>
                {job.name}
              </td>
              <td className={td}>
                <StatePill
                  state={job.state}
                  scope="job"
                  size="sm"
                  cancelling={isCancelling(job.state, job.cancel_requested_at)}
                />
              </td>
              <td className={cn(td, "tnum text-right text-muted")}>{job.steps.length}</td>
              <td className={cn(td, "tnum whitespace-nowrap text-muted")}>
                {formatInstant(job.created_at)}
              </td>
              <td className={cn(td, "tnum text-right text-muted")}>
                {job.duration_ms === null ? (
                  <span className="text-faint">—</span>
                ) : (
                  formatDuration(job.duration_ms)
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
