/**
 * The time scale, and the collapsed idle gutter that makes this chart possible.
 *
 * ---------------------------------------------------------------------------
 * THE PROBLEM, stated as the two numbers that have to share one viewport.
 *
 * An `echo` step finishes in about 3 milliseconds. A step that has failed
 * twice waits out an exponential backoff that is routinely tens of minutes.
 * Both appear in the same job, on the same axis. On a linear scale 1400px
 * wide, forty minutes maps every millisecond to 0.0000006px: the echo step is
 * a bar 0.002 pixels wide, which is nothing, and 95% of the ink on the screen
 * is empty space where the runtime was deliberately idle.
 *
 * A log scale is the textbook answer and it is wrong here, because a log axis
 * has no origin an operator can reason about and the durations being compared
 * are wall-clock instants, not magnitudes. What is actually wanted is to spend
 * pixels where work happened and to spend a fixed, marked, honest allowance
 * where nothing did. So this scale is piecewise linear: every interval in
 * which some step was claimed or executing is scaled normally against a single
 * shared pixels-per-millisecond, and every interval longer than the idle
 * threshold in which nothing was, collapses to a fixed-width gutter that the
 * chart draws as a zigzag rule labelled with the time it swallowed.
 *
 * The result is a chart where proportions WITHIN active periods are true and
 * comparable, and the dead time is visible as dead time rather than as an
 * expanse of nothing. `mode: "linear"` is one keystroke away and is what an
 * operator switches to when they want real wall-clock proportion — which is a
 * different question, honestly answered by a different chart.
 * ---------------------------------------------------------------------------
 *
 * Pure. No DOM, no clock, no React. Every function here is total: an empty
 * domain, a zero width, a job with no work at all and a domain of zero
 * duration all produce a usable scale rather than NaN, because a NaN that
 * reaches an SVG attribute is dropped by the browser with no error at all.
 */

/** An interval the scale collapsed, in both time and pixels. */
export interface IdleBand {
  /** Epoch ms. */
  from: number;
  to: number;
  /** Pixel offset of the gutter's left edge. */
  x: number;
  /** Always `gutterPx`. Carried so a caller never recomputes it. */
  width: number;
  /** The wall-clock duration this gutter stands in for. */
  elidedMs: number;
}

export interface Tick {
  /** Epoch ms. */
  t: number;
  x: number;
}

/** A pixel span, with the flag that says the chart is lying about its size. */
export interface Span {
  x: number;
  width: number;
  /**
   * The true duration mapped to fewer than `barMin` pixels and was widened.
   *
   * This is not cosmetic. A 3ms step and a fail-fast sweep that stamps twenty
   * steps with the identical instant both produce sub-pixel bars, and a bar
   * that is not drawn is a bar that cannot be hovered, focused or clicked —
   * the step disappears from the chart and from the keyboard order. So it is
   * widened to be reachable, and the flag travels with it so the renderer can
   * mark the edge and the hover card can print the true duration in
   * milliseconds. The chart has to distort that segment; it does not have to
   * hide that it did.
   */
  clamped: boolean;
}

export type ScaleMode = "linear" | "compress";

export interface Scale {
  mode: ScaleMode;
  domain: [number, number];
  width: number;
  barMin: number;
  /** Epoch ms to pixels. Clamped to the domain at both ends. */
  x(t: number): number;
  /** Pixels back to epoch ms. Inside a gutter this interpolates across the
   *  elided range, which is the honest inverse: the gutter really does stand
   *  for that stretch of time, compressed. */
  invert(px: number): number;
  /** A ready-to-draw pixel span, with the minimum-width clamp applied. */
  span(from: number, to: number): Span;
  bands: IdleBand[];
  ticks: Tick[];
  /** The chosen tick spacing in ms, for the `--grid-step` custom property. */
  tickStepMs: number;
  /** Average pixels per millisecond across the ACTIVE portion. Zero for a
   *  degenerate scale, and every caller must tolerate that. */
  pxPerMs: number;
}

interface Piece {
  from: number;
  to: number;
  x: number;
  width: number;
  /** An idle gutter: fixed width, time compressed. */
  gutter: boolean;
}

export interface ScaleOptions {
  domain: [number, number];
  width: number;
  mode?: ScaleMode;
  /** Intervals in which some step was claimed or executing, from
   *  `busyIntervals`. Merged and sorted is expected but not required. */
  busy?: ReadonlyArray<readonly [number, number]>;
  /** Shorter idle stretches than this are left alone. Five seconds, because
   *  below that the gutter costs more pixels than the gap it replaces and the
   *  chart starts to look shattered. */
  idleThresholdMs?: number;
  gutterPx?: number;
  barMin?: number;
  /** Target pixels between gridlines. The 1/2/5 ladder picks the nearest
   *  round duration to this, never a raw division. */
  targetTickPx?: number;
}

/**
 * The tick ladder.
 *
 * Explicit rather than computed from powers of ten, because time is not
 * decimal: the round numbers a reader recognises are 15s, 30s, 5m, 15m, 1h,
 * and a 1/2/5 ladder over milliseconds produces 20s and 50m, which are not
 * durations anyone thinks in. Labels landing on values an operator already has
 * in their head is most of what makes an axis readable.
 */
const TICK_LADDER_MS = [
  1, 2, 5, 10, 25, 50, 100, 250, 500,
  1_000, 2_000, 5_000, 10_000, 15_000, 30_000,
  60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000,
  3_600_000, 7_200_000, 21_600_000, 43_200_000, 86_400_000,
];

function chooseTickStep(msPerPx: number, targetPx: number): number {
  const wanted = msPerPx * targetPx;
  if (!Number.isFinite(wanted) || wanted <= 0) return TICK_LADDER_MS[0];
  for (const step of TICK_LADDER_MS) if (step >= wanted) return step;
  // Past a day, fall back to whole days so the ladder does not run out on a
  // job that somehow ran for a week.
  return Math.ceil(wanted / 86_400_000) * 86_400_000;
}

/** Clip, merge and sort the busy intervals against the domain. */
function normaliseBusy(
  busy: ReadonlyArray<readonly [number, number]>,
  start: number,
  end: number,
): Array<[number, number]> {
  const clipped: Array<[number, number]> = [];
  for (const [from, to] of busy) {
    if (!Number.isFinite(from) || !Number.isFinite(to)) continue;
    const lo = Math.max(start, Math.min(from, to));
    const hi = Math.min(end, Math.max(from, to));
    if (hi >= lo) clipped.push([lo, hi]);
  }
  clipped.sort((a, b) => a[0] - b[0]);

  const merged: Array<[number, number]> = [];
  for (const [from, to] of clipped) {
    const last = merged[merged.length - 1];
    if (last && from <= last[1]) last[1] = Math.max(last[1], to);
    else merged.push([from, to]);
  }
  return merged;
}

export function makeScale(options: ScaleOptions): Scale {
  const {
    domain,
    width,
    mode = "compress",
    busy = [],
    idleThresholdMs = 5_000,
    gutterPx = 24,
    barMin = 3,
    targetTickPx = 120,
  } = options;

  const start = domain[0];
  // A zero-width or inverted domain is not a caller error worth throwing over
  // — it is what a job with one instantaneous step produces — so it is widened
  // here to the smallest value that keeps every division below finite.
  const end = domain[1] > domain[0] ? domain[1] : domain[0] + 1;
  const plotWidth = Number.isFinite(width) && width > 0 ? width : 0;

  const pieces: Piece[] = [];
  const bands: IdleBand[] = [];

  if (mode === "linear") {
    pieces.push({ from: start, to: end, x: 0, width: plotWidth, gutter: false });
  } else {
    // The idle stretches are the COMPLEMENT of the busy set inside the domain.
    const active = normaliseBusy(busy, start, end);
    const idle: Array<[number, number]> = [];
    let cursor = start;
    for (const [from, to] of active) {
      if (from - cursor >= idleThresholdMs) idle.push([cursor, from]);
      cursor = Math.max(cursor, to);
    }
    if (end - cursor >= idleThresholdMs) idle.push([cursor, end]);

    // A job in which nothing was ever busy — every step cancelled before it
    // started — would otherwise collapse its entire domain into one gutter and
    // leave no pixels at all for the terminal ticks that are the only thing on
    // the chart. Keeping the whole span linear is the degenerate-but-correct
    // answer.
    const totalIdleMs = idle.reduce((sum, [from, to]) => sum + (to - from), 0);
    const activeMs = end - start - totalIdleMs;
    if (idle.length === 0 || activeMs <= 0) {
      pieces.push({ from: start, to: end, x: 0, width: plotWidth, gutter: false });
    } else {
      // Gutters can, on a pathological job, ask for more pixels than the chart
      // has. Shrinking them proportionally is better than letting the active
      // pieces take a negative width, which maps every bar to the same x.
      const wantedGutter = gutterPx * idle.length;
      const gutterWidth =
        wantedGutter > plotWidth * 0.6
          ? (plotWidth * 0.6) / idle.length
          : gutterPx;
      const pxPerMs = (plotWidth - gutterWidth * idle.length) / activeMs;

      let x = 0;
      let t = start;
      for (const [from, to] of idle) {
        if (from > t) {
          const pieceWidth = (from - t) * pxPerMs;
          pieces.push({ from: t, to: from, x, width: pieceWidth, gutter: false });
          x += pieceWidth;
        }
        pieces.push({ from, to, x, width: gutterWidth, gutter: true });
        bands.push({ from, to, x, width: gutterWidth, elidedMs: to - from });
        x += gutterWidth;
        t = to;
      }
      if (t < end) {
        pieces.push({ from: t, to: end, x, width: (end - t) * pxPerMs, gutter: false });
      }
    }
  }

  function x(t: number): number {
    if (!Number.isFinite(t)) return 0;
    if (t <= start) return 0;
    if (t >= end) return plotWidth;
    for (const piece of pieces) {
      if (t >= piece.from && t <= piece.to) {
        const span = piece.to - piece.from;
        if (span <= 0) return piece.x;
        return piece.x + ((t - piece.from) / span) * piece.width;
      }
    }
    return plotWidth;
  }

  function invert(px: number): number {
    if (!Number.isFinite(px)) return start;
    if (px <= 0) return start;
    if (px >= plotWidth) return end;
    for (const piece of pieces) {
      if (px >= piece.x && px <= piece.x + piece.width) {
        if (piece.width <= 0) return piece.from;
        return piece.from + ((px - piece.x) / piece.width) * (piece.to - piece.from);
      }
    }
    return end;
  }

  function span(from: number, to: number): Span {
    const lo = Math.min(from, to);
    const hi = Math.max(from, to);
    const left = x(lo);
    const right = x(hi);
    const raw = right - left;
    if (raw >= barMin) return { x: left, width: raw, clamped: false };
    // Widen leftward when the segment is up against the right edge, so a step
    // that finished at the very end of the job does not spill outside the plot.
    const overflow = left + barMin - plotWidth;
    const adjusted = overflow > 0 ? Math.max(0, left - overflow) : left;
    return { x: adjusted, width: barMin, clamped: true };
  }

  // Ticks are spaced by TIME, chosen from the ladder against the density of
  // the active pieces — so on a compressed scale the labels stay evenly spaced
  // through the parts that matter and simply skip the gutters. Emitting a tick
  // inside a gutter would print a timestamp on a zigzag that stands for a
  // range, which is the one place on this axis a single instant is meaningless.
  const activePieces = pieces.filter((piece) => !piece.gutter);
  const activeMsTotal = activePieces.reduce((sum, piece) => sum + (piece.to - piece.from), 0);
  const activePxTotal = activePieces.reduce((sum, piece) => sum + piece.width, 0);
  const msPerPx = activePxTotal > 0 ? activeMsTotal / activePxTotal : (end - start) / Math.max(plotWidth, 1);
  const tickStepMs = chooseTickStep(msPerPx, targetTickPx);

  const ticks: Tick[] = [];
  if (plotWidth > 0) {
    for (const piece of activePieces) {
      const firstTick = Math.ceil(piece.from / tickStepMs) * tickStepMs;
      for (let t = firstTick; t <= piece.to; t += tickStepMs) {
        ticks.push({ t, x: x(t) });
        // A domain spanning years with a 1ms ladder cannot happen — the ladder
        // tops out at whole days — but an unbounded loop over a browser main
        // thread is not a risk worth carrying for a chart.
        if (ticks.length > 512) break;
      }
      if (ticks.length > 512) break;
    }
  }

  return {
    mode,
    domain: [start, end],
    width: plotWidth,
    barMin,
    x,
    invert,
    span,
    bands,
    ticks,
    tickStepMs,
    pxPerMs: msPerPx > 0 ? 1 / msPerPx : 0,
  };
}
