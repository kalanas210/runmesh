"use client";

import { cn } from "@/lib/cn";
import { STATE_VAR } from "@/lib/state";
import { StateGlyph } from "@/components/ui/state-glyph";
import type { Lane } from "@/lib/waterfall";

/**
 * The left gutter cell: what this lane is, and why it is not moving.
 *
 * 264px, fixed. It is the one part of the chart that does not scale with the
 * viewport, because a step id is a fixed number of characters and squeezing it
 * buys a few pixels of plot at the cost of the only human-readable identifier
 * on the row. Overflow is masked rather than ellipsised — `.mask-fade-r` fades
 * the last few characters out, which reads as "there is more" without stealing
 * three character cells for a "…" on every single row.
 *
 * The glyph is here, in the label, and not on the bar. A bar already carries
 * its state through colour AND texture AND position; a third carrier inside a
 * 28px row would be noise. In the label it sits in the same column on every
 * row, which makes a column of glyphs scannable in a way a column of colours
 * alone is not.
 */
export function LaneLabel({
  lane,
  selected,
  dimmed,
  isDependency,
  isDependent,
  className,
}: {
  lane: Lane;
  selected: boolean;
  dimmed: boolean;
  /** A direct dependency of the selected lane. */
  isDependency: boolean;
  /** A direct dependent of the selected lane: marked with a left tick. */
  isDependent: boolean;
  className?: string;
}) {
  const attempts = lane.attempts.length;

  return (
    <div
      className={cn(
        "relative flex h-full min-w-0 items-center gap-2 pl-3 pr-2 transition-opacity",
        dimmed && "opacity-40",
        className,
      )}
      style={{ transitionDuration: "var(--dur-hover)" }}
    >
      {/* The dependency marks are strokes, not colours: a 2px accent tick for a
          dependent and a hairline for a dependency. They read at a glance and
          they read without hue. */}
      {(selected || isDependent) && (
        <span
          aria-hidden
          className={cn(
            "absolute inset-y-0 left-0",
            selected ? "w-[2px] bg-tint" : "w-[2px] bg-accent/60",
          )}
        />
      )}
      {isDependency && !selected && (
        <span aria-hidden className="absolute inset-y-0 left-0 w-px bg-accent/35" />
      )}

      <span style={{ color: `var(${STATE_VAR[lane.state]})` }}>
        <StateGlyph state={lane.state} />
      </span>

      <span className="mask-fade-r min-w-0 flex-1 overflow-hidden whitespace-nowrap">
        <span className="tnum text-[0.7rem] leading-none text-bone-2">{lane.stepId}</span>
        <span className="ml-2 text-[0.6rem] leading-none text-faint">{lane.tool}</span>
      </span>

      {/* blocked_by is derived per request and stored nowhere, so this chip is
          only ever as fresh as the snapshot that carried it. It is shown while
          the step is QUEUED because that is the one moment the answer to "why
          has this not started" is worth a chip rather than a click. */}
      {lane.state === "QUEUED" && lane.blockedBy.length > 0 && (
        <span
          className="tnum shrink-0 rounded-full border border-dotted border-st-queued/45 px-1.5 text-[0.55rem] leading-[1.4] text-st-queued"
          title={`blocked by ${lane.blockedBy.join(", ")}`}
        >
          ⟂{lane.blockedBy.length}
        </span>
      )}

      {/* The attempt counter appears only when there is more than one, so the
          common case carries no ink at all. `n/max` rather than a bare count:
          "3/3" says the budget is spent, which "3" does not. */}
      {attempts > 1 && (
        <span className="tnum shrink-0 text-[0.55rem] leading-none text-st-retrying">
          {attempts}/{lane.maxAttempts}
        </span>
      )}
    </div>
  );
}
