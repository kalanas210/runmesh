"use client";

import type { Lane } from "@/lib/waterfall";

/** Where a lane's drawn work begins and ends, in pixels. */
export interface LaneEdges {
  startX: number;
  endX: number;
}

/**
 * Dependency edges, drawn ONLY for the selected lane.
 *
 * The default for this overlay is to draw nothing, and that is the single
 * decision that keeps this chart readable. A fan-out plan — the heuristic
 * planner emits N fetches into an analyze into a report — produces an
 * unreadable weave at even ten steps if every edge is a line, and at a hundred
 * steps the edges are the only thing on the screen. The DAG's shape is carried
 * statically instead, by the topological depth bands and by the `blocked_by`
 * chip in the lane label, and neither costs a pixel of ink.
 *
 * What an operator actually asks is never "show me the graph"; it is "what was
 * this one step waiting for". That question has a small answer — fan-in for a
 * single step is almost always one to three — so the on-demand set is always
 * readable where the all-pairs set never is.
 *
 * Orthogonal elbows, not bezier curves. A curve between two rows 28px apart
 * has nowhere to put its control points and ends up as an ambiguous blob
 * exactly where two edges cross; a right-angled route is traceable by eye
 * because every crossing is a clean perpendicular.
 */
export function DependencyOverlay({
  lane,
  edges,
  laneMid,
  width,
  height,
}: {
  /** The selected lane. Its direct dependencies get an incoming edge. */
  lane: Lane;
  edges: ReadonlyMap<string, LaneEdges>;
  laneMid: ReadonlyMap<string, number>;
  width: number;
  height: number;
}) {
  const target = edges.get(lane.stepId);
  const targetY = laneMid.get(lane.stepId);
  if (!target || targetY === undefined || width <= 0) return null;

  const paths: Array<{ id: string; d: string; x: number; y: number }> = [];
  for (const dependency of lane.dependsOn) {
    const source = edges.get(dependency);
    const sourceY = laneMid.get(dependency);
    if (!source || sourceY === undefined) continue;

    const x1 = source.endX;
    const x2 = target.startX;
    // The turn is taken just past the dependency's own end, so a vertical run
    // never sits on top of a bar it has nothing to do with.
    const turn = Math.min(Math.max(x1 + 8, 4), Math.max(width - 4, 4));
    paths.push({
      id: dependency,
      d: `M ${x1} ${sourceY} L ${turn} ${sourceY} L ${turn} ${targetY} L ${x2} ${targetY}`,
      x: x2,
      y: targetY,
    });
  }

  if (paths.length === 0) return null;

  return (
    <svg
      className="pointer-events-none absolute inset-0"
      width={width}
      height={height}
      aria-hidden
    >
      {paths.map((path) => (
        <g key={path.id}>
          <path
            d={path.d}
            fill="none"
            stroke="var(--accent-line)"
            strokeWidth="1"
            strokeLinejoin="round"
          />
          {/* A dot rather than an arrowhead: at 1px stroke weight a triangle
              needs five pixels to read as a direction and there are rarely
              five spare, whereas a 2.5px dot lands unambiguously on the bar it
              points at. Direction is never in doubt anyway — the edge always
              runs from a shallower band to a deeper one. */}
          <circle cx={path.x} cy={path.y} r="2.5" fill="var(--accent)" />
        </g>
      ))}
    </svg>
  );
}
