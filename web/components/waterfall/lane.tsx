"use client";

import { cn } from "@/lib/cn";
import type { Scale } from "@/lib/scale";
import { attemptAt, type Lane, type Segment } from "@/lib/waterfall";
import { LaneLabel } from "./lane-label";
import { SegmentBar } from "./attempt-bar";
import { GUTTER_W, LANE_GAP, LANE_H } from "./geometry";

/**
 * One row: a step, its label, and every interval its attempts occupied.
 *
 * THE ROW IS THE FOCUSABLE UNIT, and that is the single decision that makes
 * this chart usable from a keyboard. A hundred-step job with three attempts
 * each has three hundred segments; making each of them a tab stop would put
 * three hundred stops between the chart and whatever follows it, which is not
 * an accessible chart, it is a trap. So the row takes focus, arrow keys move
 * along its attempts, and the `aria-label` says the whole row as a sentence.
 * `describeLane` builds that sentence in the pure module, where it is tested.
 *
 * The label cell is `sticky left-0` rather than a separate column, so the row
 * is ONE element in the DOM. The alternative — a gutter column beside a plot
 * column — makes the row a fiction that exists only visually, which means the
 * grid has no rows, `aria-label` has nothing to sit on, and a screen reader
 * reads a hundred step ids followed by a hundred unrelated bars.
 */
export function LaneRow({
  lane,
  segments,
  scale,
  rowIndex,
  ariaLabel,
  selected,
  selectedAttempt,
  dimmed,
  isDependency,
  isDependent,
  hovered,
  tabbable,
  rowRef,
  onHover,
  onSelect,
  onFocus,
}: {
  lane: Lane;
  segments: readonly Segment[];
  scale: Scale;
  /** 1-based, and counted over band headers too, because aria-rowindex is a
   *  position within the grid rather than within the lanes. */
  rowIndex: number;
  ariaLabel: string;
  selected: boolean;
  /** Which attempt of this lane is selected, if any. Drawn a step brighter than
   *  its neighbours so ←/→ has something visible to move. */
  selectedAttempt: number | null;
  /** Not a dependency, not a dependent, and something else is selected. */
  dimmed: boolean;
  isDependency: boolean;
  isDependent: boolean;
  /** The segment under the pointer, when it is in this lane. */
  hovered: Segment | null;
  /** The single tab stop of the grid. Roving, so the chart costs one stop. */
  tabbable: boolean;
  rowRef: (element: HTMLDivElement | null) => void;
  onHover: (lane: Lane, segment: Segment | null) => void;
  onSelect: (lane: Lane, segment: Segment | null) => void;
  onFocus: (lane: Lane) => void;
}) {
  // The number only appears on a lane that has more than one attempt, so the
  // common case — which is almost every lane on almost every job — carries no
  // extra ink at all.
  const showAttemptNumber = lane.attempts.length > 1;

  return (
    <div
      ref={rowRef}
      role="row"
      aria-rowindex={rowIndex}
      aria-selected={selected}
      aria-label={ariaLabel}
      tabIndex={tabbable ? 0 : -1}
      data-step={lane.stepId}
      className={cn(
        "group relative flex outline-none",
        "focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-0",
        selected && "bg-bone/[0.04]",
      )}
      style={{
        height: LANE_H,
        // The gap is written HERE, on the row, rather than as a row-gap on the
        // container: the container also holds the band headers, which must not
        // be followed by one, and `layout()` in geometry.ts has already
        // committed to LANE_H + LANE_GAP when it told the overlay where this
        // lane's centre line is.
        marginBottom: LANE_GAP,
        // Selection tints the row rather than recolouring it: --tint is the
        // ApexTick per-subtree override, so the accent reaches the label rail
        // without any component below having to know that it is selected.
        ...(selected ? ({ ["--tint" as string]: "var(--accent)" } as React.CSSProperties) : null),
      }}
      onFocus={() => onFocus(lane)}
      onMouseLeave={() => onHover(lane, null)}
    >
      {/* The new-event cue. Always in the DOM and invisible; `useEventFlash`
          sets `data-flash` on the ROW and the `.lane-flash` rule in globals.css
          runs it. Rendering it conditionally would mean re-rendering a hundred
          lanes to fade one 2px rail, and under prefers-reduced-motion the
          global block turns the same attribute into a static inset rule on the
          row — the cue degrades to something still visible rather than to
          nothing, because it says WHAT CHANGED and that is not decoration. */}
      <span aria-hidden className="lane-flash" />

      <div
        role="gridcell"
        className="sticky left-0 z-10 shrink-0 border-r border-line bg-ink"
        style={{ width: GUTTER_W }}
      >
        <LaneLabel
          lane={lane}
          selected={selected}
          dimmed={dimmed}
          isDependency={isDependency}
          isDependent={isDependent}
        />
      </div>

      <div
        role="gridcell"
        className={cn("relative min-w-0 flex-1 transition-opacity", dimmed && "opacity-40")}
        style={{ transitionDuration: "var(--dur-hover)" }}
        onClick={(event) => {
          // A click on the row's empty space selects the LANE without an
          // attempt. That is a real and useful selection — "show me this
          // step's dependencies" — and without it the only way to select a
          // lane whose bars are 3px wide is to hit a 3px target.
          //
          // The target check is load-bearing rather than defensive: a click on
          // a segment fires the segment's own handler and then BUBBLES here,
          // so without it every attempt selection would be immediately
          // replaced by a bare lane selection and ←/→ would never have an
          // attempt to move from.
          if (event.target === event.currentTarget) onSelect(lane, null);
        }}
      >
        {segments.map((segment, index) => (
          <SegmentBar
            key={`${segment.kind}:${segment.attempt}:${segment.from}:${index}`}
            segment={segment}
            attempt={attemptAt(lane, segment)}
            lane={lane}
            scale={scale}
            active={
              hovered === segment ||
              (selected && selectedAttempt !== null && segment.attempt === selectedAttempt)
            }
            showAttemptNumber={showAttemptNumber}
            onHover={(next) => onHover(lane, next)}
            onSelect={(next) => onSelect(lane, next)}
          />
        ))}
      </div>
    </div>
  );
}
