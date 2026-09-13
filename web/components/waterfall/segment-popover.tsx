"use client";

import { formatDuration, formatInstant } from "@/lib/format";
import { StatePill } from "@/components/ui/state-pill";
import type { Scale } from "@/lib/scale";
import type { Attempt, Lane, Segment } from "@/lib/waterfall";
import { LANE_H } from "./geometry";

/**
 * The hover card, anchored to the SEGMENT and never to the cursor.
 *
 * A cursor-following tooltip on a 28px row is unreadable for a structural
 * reason rather than a taste one: the card is taller than the row, so it sits
 * under the pointer, so moving the pointer toward it dismisses it, and the
 * numbers in it shift as the hand moves. Anchoring to the segment with a fixed
 * 8px offset gives a card that is still there when the eye arrives.
 *
 * `pointer-events-none` because the card overlaps neighbouring lanes, and a
 * card that swallows a hover would make the lane underneath unreachable.
 * Everything in it is also in the table below the chart, so nothing here is
 * the only route to a fact.
 */
export function SegmentPopover({
  lane,
  attempt,
  segments,
  segment,
  scale,
  laneTop,
  bodyHeight,
}: {
  lane: Lane;
  attempt?: Attempt;
  /** Every segment of this attempt, so the three spans can be broken out. */
  segments: readonly Segment[];
  segment: Segment;
  scale: Scale;
  laneTop: number;
  bodyHeight: number;
}) {
  const span = scale.span(segment.from, segment.to);
  const width = 268;

  // Clamped to the plot at both ends. A card that opens past the right edge is
  // clipped by the chart's own overflow and loses the error message, which is
  // the half a reader opened it for.
  const left = Math.min(Math.max(span.x - 4, 0), Math.max(scale.width - width, 0));
  // Below the lane by default, above it when there is no room — measured
  // against the body rather than the viewport, because the chart scrolls
  // inside the page and the viewport says nothing useful about it.
  const below = laneTop + LANE_H + 8;
  const flip = below + 190 > bodyHeight;
  const top = flip ? Math.max(laneTop - 190, 0) : below;

  const queuedMs = totalOf(segments, "queued");
  const pendingMs = totalOf(segments, "pending");
  const runMs = totalOf(segments, "run");
  const anyInferred = segments.some((entry) => entry.kind === "queued" && entry.inferred);

  return (
    <div
      role="tooltip"
      className="pointer-events-none absolute z-30 rounded-xl border border-line-2 bg-ink-3 p-3 shadow-[0_18px_40px_-18px_rgba(0,0,0,0.9)]"
      style={{ left, top, width }}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="tnum truncate text-[0.7rem] text-bone">{lane.stepId}</span>
        <StatePill state={attempt?.state ?? lane.state} size="sm" />
      </div>
      <div className="mt-0.5 text-[0.6rem] text-faint">{lane.tool}</div>

      <dl className="mt-2.5 space-y-1 text-[0.65rem]">
        {attempt && (
          <Row
            k="attempt"
            v={
              <>
                {attempt.n} of {lane.maxAttempts}
                {attempt.failures !== undefined && (
                  // Attempt and failures are different numbers and the UI must
                  // not conflate them: a release or a lease expiry re-claims
                  // without spending budget, so attempt 4 with failures 1 is a
                  // correct and common pair.
                  <span className="text-faint"> · {attempt.failures} failed</span>
                )}
              </>
            }
          />
        )}

        <Row
          k="spans"
          v={
            <span className="tnum">
              {formatDuration(queuedMs)} queued
              {anyInferred && <span className="text-faint"> (inferred)</span>} ·{" "}
              {formatDuration(pendingMs)} pending · {formatDuration(runMs)} ran
            </span>
          }
        />

        {span.clamped && (
          // The bar was widened to the 3px minimum so it could be hovered at
          // all, so the true number has to be here and has to be exact.
          <Row
            k="true size"
            v={
              <span className="tnum text-st-retrying">
                {formatDuration(segment.to - segment.from, true)} — bar widened to stay
                reachable
              </span>
            }
          />
        )}

        {attempt?.durationMs !== undefined && (
          <Row k="duration_ms" v={<span className="tnum">{attempt.durationMs}</span>} />
        )}

        {attempt?.backoffUntil !== undefined && (
          <Row
            k="backoff until"
            v={<span className="tnum">{formatInstant(new Date(attempt.backoffUntil).toISOString())}</span>}
          />
        )}

        {attempt?.expiredOwner !== undefined && (
          <Row k="lease owner" v={<span className="tnum">{attempt.expiredOwner}</span>} />
        )}
        {attempt?.releasedReason !== undefined && (
          <Row k="released" v={<span className="tnum">{attempt.releasedReason}</span>} />
        )}

        <Row
          k="from"
          v={<span className="tnum">{formatInstant(new Date(segment.from).toISOString())}</span>}
        />
        <Row
          k="to"
          v={
            <span className="tnum">
              {segment.open ? "still open" : formatInstant(new Date(segment.to).toISOString())}
            </span>
          }
        />
      </dl>

      {attempt?.error && (
        <div className="mt-2 border-t border-line pt-2">
          <div className="flex items-center gap-1.5">
            <span className="tnum text-[0.6rem] text-st-failed">{attempt.error.code}</span>
            <span className="text-[0.55rem] text-faint">
              {attempt.error.retryable ? "retryable" : "terminal"}
            </span>
          </div>
          <p className="mt-1 text-[0.65rem] leading-snug text-muted">{attempt.error.message}</p>
        </div>
      )}
    </div>
  );
}

function Row({ k, v }: { k: string; v: React.ReactNode }) {
  return (
    <div className="flex gap-2">
      <dt className="w-[5.5rem] shrink-0 text-faint">{k}</dt>
      <dd className="min-w-0 flex-1 text-bone-2">{v}</dd>
    </div>
  );
}

function totalOf(segments: readonly Segment[], kind: Segment["kind"]): number {
  let total = 0;
  for (const segment of segments) if (segment.kind === kind) total += segment.to - segment.from;
  return total;
}
