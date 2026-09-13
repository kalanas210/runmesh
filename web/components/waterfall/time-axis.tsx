"use client";

import { cn } from "@/lib/cn";
import { formatDuration, formatTimeOfDay } from "@/lib/format";
import type { Scale } from "@/lib/scale";
import { AXIS_H } from "./geometry";

/**
 * The sticky time axis.
 *
 * Absolute UTC wall-clock, not elapsed time, and in tabular numerals. Both
 * choices are about what an operator does next with the number: they take it
 * to a log line, a pod event or an incident timeline, and every one of those
 * is in absolute UTC. An axis reading "+00:12.4" is a number that cannot leave
 * this screen. The relative rendering does exist, but only where it has to —
 * on the narrow-viewport card list, where an absolute timestamp does not fit.
 *
 * Tabular numerals are not decoration either. The axis re-renders as the
 * window pans and as an open bar grows, and proportional digits make the
 * labels shift horizontally by a pixel or two each time, which reads as the
 * whole axis trembling.
 */
export function TimeAxis({
  scale,
  cursor,
  className,
}: {
  scale: Scale;
  /** Epoch ms under the pointer, or null. Printed in the axis so the hover
   *  rule has a readable value rather than only a position. */
  cursor?: number | null;
  className?: string;
}) {
  const cursorX = cursor == null ? null : scale.x(cursor);

  return (
    <div
      className={cn("relative select-none border-b border-line bg-ink-2", className)}
      style={{ height: AXIS_H }}
      aria-hidden
    >
      {scale.ticks.map((tick) => (
        <div
          key={tick.t}
          className="absolute bottom-0 top-0 flex items-end"
          style={{ left: tick.x }}
        >
          <span className="absolute bottom-0 top-2 w-px bg-line" />
          {/* Pulled 1px left of the gridline rather than centred on it: a
              centred label at x=0 is half outside the plot, and the first tick
              is almost always at x=0. */}
          <span className="tnum relative -mb-px ml-1.5 pb-1 text-[0.625rem] leading-none text-faint">
            {formatTimeOfDay(new Date(tick.t).toISOString())}
          </span>
        </div>
      ))}

      {/* The elided-time labels sit in the axis rather than on the gutters
          themselves, because a 24px-wide band has no room for "+40m 00s" and
          rotating it vertically makes a number that has to be read quickly
          into a puzzle. */}
      {scale.bands.map((band) => (
        <div
          key={band.from}
          className="absolute bottom-0 top-0 flex items-center justify-center overflow-visible"
          style={{ left: band.x, width: band.width }}
        >
          <span className="tnum whitespace-nowrap rounded-full border border-line-2 bg-ink px-1.5 py-px text-[0.5625rem] leading-none text-faint">
            +{formatDuration(band.elidedMs)}
          </span>
        </div>
      ))}

      {cursorX != null && cursor != null && (
        <>
          <span
            className="absolute bottom-0 top-0 w-px bg-bone/20"
            style={{ left: cursorX }}
          />
          <span
            className="tnum absolute bottom-1 rounded-sm bg-bone px-1 text-[0.625rem] leading-tight text-ink"
            style={{
              // Clamped so the readout never leaves the plot at either end —
              // at the right edge it would otherwise sit under the inspector.
              left: Math.min(Math.max(cursorX - 26, 0), Math.max(scale.width - 54, 0)),
            }}
          >
            {formatTimeOfDay(new Date(cursor).toISOString())}
          </span>
        </>
      )}
    </div>
  );
}
