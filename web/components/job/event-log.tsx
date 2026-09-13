"use client";

import { useMemo, useState } from "react";
import { newestFirst } from "@/lib/event-view";
import type { RunMeshEvent } from "@/lib/types";
import { EmptyState } from "@/components/ui/empty-state";
import { Notice } from "@/components/ui/notice";
import { EventRow } from "./event-row";

/**
 * The raw timeline, newest first.
 *
 * This is the waterfall's twin, not its appendix: the chart shows WHEN things
 * happened relative to each other, and the log shows WHAT happened in the order
 * the runtime recorded it. An operator debugging a retry storm reads the chart;
 * one asking "what did the worker actually do at 03:41:02" reads this. Neither
 * replaces the other, and both are built from the same array.
 *
 * ---------------------------------------------------------------------------
 * THE TRUNCATION BANNER IS MANDATORY.
 *
 * With RUNMESH_STORE=memory the per-job ring holds 512 events (the default
 * RUNMESH_JOB_EVENT_BUFFER) and evicts. `truncated: true` means events are
 * PERMANENTLY gone — not paged out, gone — and a log that renders the survivors
 * with no banner tells a reader the job started at the oldest surviving line.
 * On a heavily retried job, which is exactly the job somebody opens this
 * screen for, that is a confident lie.
 * ---------------------------------------------------------------------------
 *
 * The list is capped at a window rather than virtualised. A dependency for one
 * panel is a poor trade, and the cap is honest: it says how many are hidden and
 * offers to show them, instead of silently rendering the first two hundred.
 */

const WINDOW = 200;

export function EventLog({
  events,
  truncated,
  oldestSeq,
  onSelectStep,
}: {
  events: readonly RunMeshEvent[];
  truncated: boolean;
  oldestSeq: number;
  onSelectStep?: (stepId: string, attempt: number | null) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const ordered = useMemo(() => newestFirst(events), [events]);
  const shown = expanded ? ordered : ordered.slice(0, WINDOW);
  const hidden = ordered.length - shown.length;

  if (events.length === 0) {
    return (
      <EmptyState
        title="No events yet"
        hint="A job emits JOB_CREATED as its first event, so an empty timeline means the page has not arrived rather than that nothing happened."
      />
    );
  }

  return (
    <div className="rounded-2xl border border-line bg-ink-2">
      {truncated && (
        <Notice
          tone="warn"
          className="rounded-b-none border-0 border-b border-st-retrying/30"
          title={`History truncated before seq ${oldestSeq}`}
        >
          Earlier events were evicted from the in-memory ring and cannot be
          recovered. Everything before that sequence number is permanently
          missing from both this log and the waterfall. A durable store keeps
          the whole history; see Runtime for which one this process is using.
        </Notice>
      )}

      <ol className="max-h-[32rem] overflow-y-auto">
        {shown.map((event) => (
          // The per-job seq is gap-free and unique, which makes it the only key
          // here that cannot collide. global_seq is store-wide and its
          // allocation order is not its commit order under PostgreSQL.
          <EventRow key={event.seq} event={event} onSelectStep={onSelectStep} />
        ))}
      </ol>

      {hidden > 0 && (
        <button
          type="button"
          onClick={() => setExpanded(true)}
          className="w-full border-t border-line px-4 py-2.5 text-[0.7rem] text-muted transition-colors hover:bg-bone/[0.03] hover:text-bone"
          style={{ transitionDuration: "var(--dur-hover)" }}
        >
          Show {hidden} older {hidden === 1 ? "event" : "events"}
        </button>
      )}
    </div>
  );
}
