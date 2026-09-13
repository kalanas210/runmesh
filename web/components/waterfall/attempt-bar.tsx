"use client";

import { cn } from "@/lib/cn";
import { STATE_FILL_VAR, STATE_VAR } from "@/lib/state";
import type { Scale } from "@/lib/scale";
import type { Attempt, Segment } from "@/lib/waterfall";
import { LANE_H } from "./geometry";

/**
 * One interval of one attempt, as pixels.
 *
 * Four kinds, four textures, and the texture is what carries the meaning when
 * colour does not:
 *
 *   queued   a 2px dotted baseline    eligible, unclaimed
 *   pending  a 45deg hatch block      claimed by a worker, tool not started
 *   run      a solid stroked bar      the tool was executing
 *   backoff  a dotted half-height rule  deliberately waiting between attempts
 *
 * `pending` is the one a reader will not have seen on another Gantt and it is
 * the reason this chart is worth the trouble. Under the in-process executor it
 * is microseconds and invisible. Under the Kubernetes executor it is the pod
 * scheduling and image pull, routinely seconds, and it belongs to nothing else
 * on the API: `duration_ms` excludes it by construction. Merging it into the
 * run bar would make the picture disagree with the number printed beside it.
 *
 * Nothing here is a button. Three hundred focusable segments on a hundred-step
 * job would make the chart unusable from a keyboard — the tab order alone
 * would take a minute to cross. The LANE is the focusable unit, arrow keys move
 * along its attempts, and the full text of every attempt is in the table below
 * the chart.
 */
export function SegmentBar({
  segment,
  attempt,
  lane,
  scale,
  active,
  showAttemptNumber,
  onHover,
  onSelect,
}: {
  segment: Segment;
  /** The attempt this segment belongs to. Absent for the synthetic trailing
   *  queued segment, which no attempt owns. */
  attempt?: Attempt;
  lane: { timeoutMs: number };
  scale: Scale;
  /** Hovered or selected: the same visual weight, because both mean "this is
   *  the one you are asking about". */
  active: boolean;
  showAttemptNumber: boolean;
  onHover: (segment: Segment | null) => void;
  onSelect: (segment: Segment) => void;
}) {
  const span = scale.span(segment.from, segment.to);
  const colour = `var(${STATE_VAR[segment.state]})`;
  const fill = `var(${STATE_FILL_VAR[segment.state]})`;

  const common = {
    onMouseEnter: () => onHover(segment),
    onMouseLeave: () => onHover(null),
    onClick: () => onSelect(segment),
  };

  if (segment.kind === "queued") {
    return (
      <span
        {...common}
        className="dotline absolute cursor-pointer"
        style={{
          left: span.x,
          width: span.width,
          top: LANE_H / 2 - 1,
          height: 2,
          // --hatch is what the .dotline and .hatch utilities read, so the same
          // utility paints eight different states without eight class names
          // that Tailwind would have to find in the source text.
          ["--hatch" as string]: colour,
          opacity: active ? 1 : 0.75,
        }}
      />
    );
  }

  if (segment.kind === "backoff") {
    return (
      <span
        {...common}
        className="absolute flex cursor-pointer items-center"
        style={{ left: span.x, width: span.width, top: LANE_H / 2 - 5, height: 10 }}
      >
        <span
          className="dotline absolute inset-x-0 top-[4px] h-px"
          style={{ ["--hatch" as string]: colour }}
        />
        {/* The retry marker sits at the midpoint of the wait rather than at
            either end, so it reads as "this gap is a backoff" rather than as a
            property of the attempt on one side of it. */}
        {span.width > 14 && (
          <span
            className="tnum absolute left-1/2 -translate-x-1/2 bg-ink-2 px-[3px] text-[0.5rem] leading-none"
            style={{ color: colour }}
          >
            ↻
          </span>
        )}
      </span>
    );
  }

  if (segment.kind === "tick") {
    // A step that finished without ever starting: cancelled where it stood, or
    // doomed because a dependency failed. It has one instant and no extent, so
    // it is drawn as what it is — a terminal mark — rather than as a bar of
    // zero width that no pointer could ever find.
    return (
      <span
        {...common}
        className="absolute cursor-pointer"
        style={{
          left: span.x,
          width: Math.max(span.width, 2),
          top: 4,
          height: LANE_H - 8,
          background: colour,
          opacity: active ? 1 : 0.85,
        }}
      />
    );
  }

  const isPending = segment.kind === "pending";
  const height = isPending ? 12 : 16;
  const top = (LANE_H - height) / 2;

  // The tool's own deadline, drawn only where it is about to matter or has.
  // timeout_seconds is the one duration on this API expressed in seconds and
  // as a float; lane.timeoutMs has already made that conversion once.
  const deadline =
    !isPending && attempt?.startedAt !== undefined && lane.timeoutMs > 0
      ? attempt.startedAt + lane.timeoutMs
      : null;
  const deadlineX =
    deadline != null && deadline > segment.from && deadline <= scale.domain[1]
      ? scale.x(deadline)
      : null;

  return (
    <>
      <span
        {...common}
        className={cn(
          "absolute cursor-pointer border transition-[width,background-color,border-color]",
          isPending && "hatch",
        )}
        style={{
          left: span.x,
          width: span.width,
          top,
          height,
          borderColor: colour,
          background: isPending ? "transparent" : fill,
          ["--hatch" as string]: colour,
          transitionDuration: "var(--dur-state)",
          transitionTimingFunction: "var(--ease-out-expo)",
          boxShadow: active ? `0 0 0 1px ${colour}` : undefined,
          opacity: active ? 1 : 0.9,
        }}
      >
        {/* The chart had to lie about this segment's size to keep it
            reachable, so it says so: a 1px bone tick on the left edge, and the
            hover card prints the true duration in milliseconds. */}
        {span.clamped && (
          <span aria-hidden className="absolute inset-y-0 left-0 w-px bg-bone" />
        )}

        {/* An open attempt: a bone edge sliding along the right end. It is the
            only continuous animation in the chart and it means exactly one
            thing — this is still running and the right edge is `now`. */}
        {segment.open && (
          <span
            data-shimmer
            aria-hidden
            className="absolute inset-y-0 right-0 w-6 overflow-hidden"
          >
            <span
              className="absolute inset-y-0 right-0 w-px bg-bone"
              style={{ animation: "rm-shimmer 1.6s linear infinite" }}
            />
          </span>
        )}

        {/* A lease expiry or a release did not end this attempt cleanly: the
            worker stopped answering, or the process was draining. A 3px hard
            cap says the bar was cut rather than finished. */}
        {!isPending &&
          attempt &&
          (attempt.expiredOwner !== undefined || attempt.releasedReason !== undefined) && (
            <span
              aria-hidden
              className="absolute -right-px inset-y-[-2px] w-[3px]"
              style={{ background: colour }}
            />
          )}
      </span>

      {deadlineX != null && (
        <span
          aria-hidden
          className="pointer-events-none absolute w-[2px]"
          style={{
            left: deadlineX,
            top: 2,
            height: LANE_H - 4,
            background: `var(${STATE_VAR.TIMED_OUT})`,
            opacity: attempt?.state === "TIMED_OUT" ? 0.9 : 0.28,
          }}
        />
      )}

      {/* Attempt numbering appears only on a lane that has more than one, so
          the common case carries none. A superscript rather than a prefix
          inside the bar: a 3px bar has no room for a character. */}
      {showAttemptNumber && !isPending && span.width >= 2 && (
        <span
          className="tnum pointer-events-none absolute text-[0.5rem] leading-none text-faint"
          style={{ left: span.x, top: 0 }}
        >
          {segment.attempt}
        </span>
      )}
    </>
  );
}
