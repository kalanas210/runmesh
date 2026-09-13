import type { DepthBand, Lane } from "@/lib/waterfall";

/**
 * The chart's vertical geometry, in one place and in numbers.
 *
 * These mirror the `--lane-h` / `--lane-gap` / `--axis-h` / `--brush-h` /
 * `--bar-min` tokens in globals.css, and they are applied to the DOM from HERE
 * rather than read from the tokens. That is deliberate and it is the only
 * duplication in this component.
 *
 * The reason: the dependency overlay and the hover rule are a single SVG laid
 * over every lane at once, so they have to know each lane's top edge in pixels
 * before layout happens. Measuring rows after paint would mean the edges
 * arrive one frame late and jump on every re-render; reading the custom
 * properties with getComputedStyle would mean a layout read on every pointer
 * move. Computing the positions and ALSO writing them as inline heights means
 * the numbers the overlay uses and the numbers the browser lays out are the
 * same numbers by construction, and no amount of CSS cascade can put them out
 * of step.
 */
export const LANE_H = 28;
export const LANE_GAP = 6;
/** The depth band's kicker row: "DEPTH 1 - 4 STEPS" plus its hairline. */
export const BAND_H = 26;
export const AXIS_H = 32;
export const BRUSH_H = 34;
export const GUTTER_W = 264;
export const BAR_MIN = 3;

/** One laid-out row: either a band header or a lane. */
export type Row =
  | { kind: "band"; depth: number; count: number; top: number; height: number }
  | { kind: "lane"; lane: Lane; top: number; height: number; ordinal: number };

export interface Layout {
  rows: Row[];
  /** Lane top edge by step id, for the overlay. */
  laneTop: Map<string, number>;
  /** Lane vertical centre by step id, for the dependency polylines. */
  laneMid: Map<string, number>;
  height: number;
  /** Lanes in visual order, which is the keyboard order. */
  order: Lane[];
}

/**
 * Lay the bands and lanes out top to bottom.
 *
 * Band headers are part of the flow rather than a sticky overlay, because a
 * band boundary is a fact about the DAG at a particular vertical position and
 * a floating one would claim the wrong lanes as it scrolled.
 */
export function layout(bands: readonly DepthBand[]): Layout {
  const rows: Row[] = [];
  const laneTop = new Map<string, number>();
  const laneMid = new Map<string, number>();
  const order: Lane[] = [];
  let top = 0;

  for (const band of bands) {
    rows.push({ kind: "band", depth: band.depth, count: band.lanes.length, top, height: BAND_H });
    top += BAND_H;
    for (const lane of band.lanes) {
      rows.push({ kind: "lane", lane, top, height: LANE_H, ordinal: order.length });
      laneTop.set(lane.stepId, top);
      laneMid.set(lane.stepId, top + LANE_H / 2);
      order.push(lane);
      top += LANE_H + LANE_GAP;
    }
  }

  return { rows, laneTop, laneMid, height: Math.max(top, LANE_H), order };
}
