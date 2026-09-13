"use client";

import { useRef, useState } from "react";
import { cn } from "@/lib/cn";
import { formatDuration } from "@/lib/format";
import { BRUSH_H } from "./geometry";

type Drag =
  | { kind: "create"; anchor: number }
  | { kind: "pan"; grab: number; from: number; to: number }
  | { kind: "edge"; edge: "from" | "to" };

/**
 * The zoom control: a linear strip of the whole job with a draggable window.
 *
 * It is deliberately LINEAR while the chart above it is compressed, and the
 * two disagreeing is the point. The brush answers "where in the job am I",
 * which is a question about wall-clock proportion — a window covering the
 * first tenth of the strip covers the first tenth of the elapsed time — while
 * the chart answers "what happened", which needs the idle stretches out of the
 * way. Making the brush compressed too would leave nothing on screen that
 * shows the real shape of the run.
 *
 * Wheel-zoom is deliberately not bound anywhere on this chart. There is no
 * smooth-scroll library here to fight, but a chart that zooms on scroll is
 * unusable inside a page that scrolls: the reader loses their place every time
 * the pointer crosses it on the way somewhere else. Zoom is this strip, and
 * the +/-/0 keys.
 */
export function OverviewBrush({
  domain,
  window: win,
  onWindow,
  busy,
  className,
}: {
  domain: [number, number];
  window: [number, number] | null;
  onWindow: (window: [number, number] | null) => void;
  /** Merged busy intervals, drawn as the density marks. */
  busy: ReadonlyArray<readonly [number, number]>;
  className?: string;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [drag, setDrag] = useState<Drag | null>(null);

  const [start, end] = domain;
  const span = Math.max(end - start, 1);

  /** Pixels within the strip to epoch ms, clamped to the domain. */
  function timeAt(clientX: number): number {
    const box = ref.current?.getBoundingClientRect();
    if (!box || box.width <= 0) return start;
    const ratio = (clientX - box.left) / box.width;
    return start + Math.min(Math.max(ratio, 0), 1) * span;
  }

  const pct = (t: number) => ((t - start) / span) * 100;

  const view = win ?? domain;
  const left = pct(view[0]);
  const right = pct(view[1]);

  function onPointerDown(event: React.PointerEvent<HTMLDivElement>) {
    const t = timeAt(event.clientX);
    event.currentTarget.setPointerCapture(event.pointerId);

    if (win) {
      const box = ref.current?.getBoundingClientRect();
      const widthPx = box?.width ?? 1;
      const edgePx = 6;
      const fromPx = ((win[0] - start) / span) * widthPx;
      const toPx = ((win[1] - start) / span) * widthPx;
      const x = event.clientX - (box?.left ?? 0);
      if (Math.abs(x - fromPx) <= edgePx) return setDrag({ kind: "edge", edge: "from" });
      if (Math.abs(x - toPx) <= edgePx) return setDrag({ kind: "edge", edge: "to" });
      if (x > fromPx && x < toPx) {
        return setDrag({ kind: "pan", grab: t, from: win[0], to: win[1] });
      }
    }
    setDrag({ kind: "create", anchor: t });
    onWindow([t, t]);
  }

  function onPointerMove(event: React.PointerEvent<HTMLDivElement>) {
    if (!drag) return;
    const t = timeAt(event.clientX);

    if (drag.kind === "create") {
      onWindow(normalise(drag.anchor, t, span));
      return;
    }
    if (drag.kind === "edge") {
      const other = drag.edge === "from" ? (win?.[1] ?? end) : (win?.[0] ?? start);
      onWindow(normalise(other, t, span));
      return;
    }
    // Pan: the window keeps its width and slides, stopping at the domain ends
    // rather than shrinking against them — a window that changed size while
    // being dragged sideways would be a zoom nobody asked for.
    const width = drag.to - drag.from;
    let from = drag.from + (t - drag.grab);
    from = Math.min(Math.max(from, start), end - width);
    onWindow([from, from + width]);
  }

  function endDrag(event: React.PointerEvent<HTMLDivElement>) {
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    // A click without a drag is a click, not a zoom to a zero-width window
    // that would divide the chart by nothing.
    if (win && win[1] - win[0] < span / 500) onWindow(null);
    setDrag(null);
  }

  return (
    <div className={cn("relative border-t border-line bg-ink", className)}>
      <div
        ref={ref}
        role="presentation"
        className="relative cursor-crosshair touch-none select-none"
        style={{ height: BRUSH_H }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onDoubleClick={() => onWindow(null)}
      >
        {busy.map(([from, to]) => (
          <span
            key={`${from}-${to}`}
            className="absolute top-2 bottom-2 bg-bone/25"
            style={{
              left: `${pct(from)}%`,
              // A minimum of one pixel: a 3ms burst in a forty-minute job is
              // 0.0001% of this strip, and a density mark nobody can see makes
              // the run look like it never happened.
              width: `max(1px, ${pct(to) - pct(from)}%)`,
            }}
          />
        ))}

        {win && (
          <>
            <span className="absolute inset-y-0 left-0 bg-ink/70" style={{ width: `${left}%` }} />
            <span
              className="absolute inset-y-0 right-0 bg-ink/70"
              style={{ width: `${100 - right}%` }}
            />
            <span
              className="absolute inset-y-0 cursor-grab border-x border-accent bg-accent-soft"
              style={{ left: `${left}%`, width: `${Math.max(right - left, 0.2)}%` }}
            />
          </>
        )}
      </div>

      <div className="flex items-center justify-between px-3 pb-1.5 pt-0.5">
        <span className="kicker text-[0.5625rem]">overview</span>
        <span className="tnum text-[0.5625rem] text-faint">
          {win
            ? `window ${formatDuration(win[1] - win[0])} of ${formatDuration(span)} — double-click to reset`
            : `full run · ${formatDuration(span)}`}
        </span>
      </div>
    </div>
  );
}

/** Order two instants and refuse a window so narrow it would divide by zero. */
function normalise(a: number, b: number, span: number): [number, number] {
  const from = Math.min(a, b);
  const to = Math.max(a, b);
  const floor = Math.max(span / 2000, 1);
  return to - from < floor ? [from, from + floor] : [from, to];
}
