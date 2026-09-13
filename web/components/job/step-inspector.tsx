"use client";

import { useMemo } from "react";
import { cn } from "@/lib/cn";
import { formatDuration, formatInstant } from "@/lib/format";
import { isCancelling } from "@/lib/state";
import type { JobResponse, RunMeshEvent } from "@/lib/types";
import {
  buildLanes,
  laneSegments,
  queuedAnchor,
  spanTotals,
  type Lane,
} from "@/lib/waterfall";
import { useNow } from "@/lib/now";
import { CopyButton } from "@/components/ui/copy-button";
import { EmptyState } from "@/components/ui/empty-state";
import { KeyValue } from "@/components/ui/kv";
import { StatePill } from "@/components/ui/state-pill";
import type { WaterfallSelection } from "@/components/waterfall/waterfall";
import { AttemptTable } from "./attempt-table";
import { ErrorChip } from "./error-chip";
import { ResultView } from "./result-view";

/**
 * Everything about one step, beside the chart.
 *
 * The chart answers "when" and this answers "what". They are driven by the same
 * selection, which lives in the URL as `?step=&attempt=` so that a reader can
 * paste a link to one failed attempt of one step into an incident channel and
 * the person who opens it sees what they saw.
 *
 * ---------------------------------------------------------------------------
 * WHERE EACH FIELD COMES FROM, BECAUSE IT IS NOT ONE PLACE.
 *
 * The STRUCTURE — depends_on, blocked_by, max_attempts, timeout, the tool, the
 * result — comes from the job snapshot, which is the only source for all of it.
 * The HISTORY — every attempt, and how long each spent claimed, pending and
 * running — comes from the event stream, because the snapshot's timestamps are
 * overwritten on every claim and describe the current attempt only.
 *
 * `blocked_by` deserves its own note: it is derived per request by
 * `Job.BlockedBy` and stored nowhere, so it is valid only for the response that
 * carried it. It is read straight off the snapshot on every render rather than
 * being cached alongside the lane, because the natural optimisation — holding
 * it in the reduced lane across a poll — shows blocking chips on steps that are
 * already unblocked.
 * ---------------------------------------------------------------------------
 */
export function StepInspector({
  job,
  events,
  selection,
  onSelect,
  onClose,
  className,
}: {
  job: JobResponse;
  events: readonly RunMeshEvent[];
  selection: WaterfallSelection | null;
  onSelect: (selection: WaterfallSelection | null) => void;
  onClose: () => void;
  className?: string;
}) {
  const now = useNow();

  const lanes = useMemo(() => buildLanes(job, events), [job, events]);
  const lane: Lane | undefined = selection
    ? lanes.find((candidate) => candidate.stepId === selection.stepId)
    : undefined;

  // The snapshot row, read fresh rather than through the lane, for the fields
  // the reducer does not carry — `result`, and the per-request `blocked_by`.
  const step = lane ? job.steps.find((candidate) => candidate.id === lane.stepId) : undefined;

  const segments = useMemo(() => {
    if (!lane) return [];
    // `now` is only needed for the open right edge of an in-flight attempt. On
    // the server there is no clock, so the job's own last-known instant stands
    // in and both renders agree.
    const clock = now ?? Date.parse(job.updated_at);
    return laneSegments(lane, { anchor: queuedAnchor(lane, job, lanes).at, now: clock });
  }, [lane, lanes, job, now]);

  const totals = useMemo(() => spanTotals(segments), [segments]);

  if (!lane || !step) {
    return (
      <aside className={cn("rounded-2xl border border-line bg-ink-2 p-6", className)}>
        <EmptyState
          className="border-0 bg-transparent p-0 text-left"
          title="Select a step to inspect its attempts"
          hint="Click a lane in the waterfall, or a step id in the timeline. The selection goes into the URL, so the link you share shows what you are looking at."
        />
      </aside>
    );
  }

  return (
    <aside
      className={cn("rounded-2xl border border-line bg-ink-2", className)}
      aria-label={`Step ${lane.stepId}`}
    >
      <header className="flex flex-wrap items-center gap-3 border-b border-line px-5 py-4">
        <div className="min-w-0">
          <p className="tnum truncate text-[0.95rem] text-bone">{lane.stepId}</p>
          <p className="mt-0.5 text-[0.7rem] text-muted">
            {lane.tool} · depth {lane.depth}
          </p>
        </div>
        <StatePill
          className="ml-auto"
          state={lane.state}
          cancelling={isCancelling(lane.state, job.cancel_requested_at)}
        />
        <button
          type="button"
          onClick={onClose}
          aria-label="Close the step inspector"
          className="rounded-full border border-line-2 px-2 py-0.5 text-[0.6rem] uppercase tracking-[0.14em] text-muted hover:border-bone hover:text-bone"
        >
          close
        </button>
      </header>

      <div className="space-y-6 px-5 py-5">
        <KeyValue
          columns={2}
          rows={[
            {
              label: "attempt",
              // Both numbers, always, because they answer different questions
              // and they disagree in the interesting cases. See AttemptTable.
              value: `${lane.attempt} of ${lane.maxAttempts} · ${lane.failures} failed`,
            },
            { label: "timeout", value: formatDuration(lane.timeoutMs) },
            {
              label: "depends on",
              value: lane.dependsOn.length > 0 ? lane.dependsOn.join(", ") : "nothing",
            },
            {
              label: "blocked by",
              // Only while it means something. A blocked_by list on a step that
              // already ran is a fact about the past that reads as the present.
              when: lane.blockedBy.length > 0,
              value: (
                <span className="text-st-queued">{lane.blockedBy.join(", ")}</span>
              ),
            },
          ]}
        />

        <section>
          <h3 className="kicker text-[0.5625rem]">where the time went</h3>
          <KeyValue
            className="mt-3"
            columns={2}
            rows={[
              {
                label: "queued",
                value: (
                  <>
                    {formatDuration(totals.queuedMs)}
                    {totals.inferred && (
                      // Nothing in the runtime records when a step became
                      // eligible — NextAttemptAt is json:"-" and only surfaces
                      // while a step is RETRYING — so this left edge is derived
                      // from the job's creation and its dependencies' ends. The
                      // word travels with the number everywhere it is printed.
                      <span className="ml-1.5 text-[0.62rem] text-faint">inferred</span>
                    )}
                  </>
                ),
              },
              {
                label: "claimed, not started",
                value: <span className="text-st-scheduled">{formatDuration(totals.pendingMs)}</span>,
              },
              { label: "tool executing", value: formatDuration(totals.runMs) },
              {
                label: "retry backoff",
                when: totals.backoffMs > 0,
                value: <span className="text-st-retrying">{formatDuration(totals.backoffMs)}</span>,
              },
            ]}
          />
        </section>

        <section>
          <h3 className="kicker text-[0.5625rem]">attempts</h3>
          <div className="mt-3">
            <AttemptTable
              attempts={lane.attempts}
              maxAttempts={lane.maxAttempts}
              selected={selection?.attempt ?? null}
              onSelect={(attempt) => onSelect({ stepId: lane.stepId, attempt })}
            />
          </div>
        </section>

        {step.error && (
          <section>
            <h3 className="kicker text-[0.5625rem]">failure</h3>
            <ErrorChip className="mt-3" error={step.error} />
          </section>
        )}

        <section>
          <h3 className="kicker text-[0.5625rem]">result</h3>
          <div className="mt-3">
            <ResultView result={step.result} />
          </div>
        </section>

        <section>
          <h3 className="kicker text-[0.5625rem]">timestamps</h3>
          <KeyValue
            className="mt-3"
            rows={[
              { label: "claimed at", value: formatInstant(step.scheduled_at) },
              { label: "started at", value: formatInstant(step.started_at) },
              { label: "ended at", value: formatInstant(step.ended_at) },
              {
                label: "next attempt at",
                // Populated ONLY while RETRYING (dto.go:159), deliberately, so
                // that no UI can render a countdown for a step that is not
                // waiting for one.
                when: step.next_attempt_at !== null,
                value: formatInstant(step.next_attempt_at),
              },
            ]}
          />
          <p className="mt-3 text-[0.65rem] leading-snug text-faint">
            These are the CURRENT attempt&rsquo;s timestamps. A claim overwrites
            them, so the per-attempt history above is the only record of earlier
            executions.
          </p>
        </section>

        <div className="flex flex-wrap gap-2 border-t border-line pt-4">
          <CopyButton value={lane.stepId} label="copy step id" />
          <CopyButton value={`${job.id}:${lane.stepId}`} label="copy job:step" />
        </div>
      </div>
    </aside>
  );
}
