import { describe, expect, it } from "vitest";
import { makeScale } from "./scale";

/**
 * The scale is the other half of the reducer's correctness: the reducer makes
 * sure the right intervals exist, and this makes sure they are all reachable
 * with a mouse and with a keyboard once they are pixels.
 *
 * The case that drives the whole design is at the bottom — a 3ms step and a
 * forty-minute backoff in one 1400px viewport — and it is not an exaggerated
 * fixture. `echo` really does return in single-digit milliseconds and the
 * default retry policy really does back off into the tens of minutes.
 */

const T0 = Date.parse("2026-09-12T03:00:00.000Z");

describe("linear mode", () => {
  const scale = makeScale({
    domain: [T0, T0 + 10_000],
    width: 1000,
    mode: "linear",
  });

  it("maps the domain onto the full width", () => {
    expect(scale.x(T0)).toBe(0);
    expect(scale.x(T0 + 10_000)).toBe(1000);
    expect(scale.x(T0 + 5_000)).toBe(500);
  });

  it("clamps outside the domain rather than drawing off the plot", () => {
    expect(scale.x(T0 - 60_000)).toBe(0);
    expect(scale.x(T0 + 60_000)).toBe(1000);
  });

  it("inverts", () => {
    expect(scale.invert(0)).toBe(T0);
    expect(scale.invert(500)).toBe(T0 + 5_000);
    expect(scale.invert(1000)).toBe(T0 + 10_000);
  });

  it("collapses nothing", () => {
    expect(scale.bands).toEqual([]);
  });
});

describe("compress mode", () => {
  // Two seconds of work, then a ten-minute idle stretch, then two more.
  const busy: Array<[number, number]> = [
    [T0, T0 + 2_000],
    [T0 + 602_000, T0 + 604_000],
  ];
  const scale = makeScale({
    domain: [T0, T0 + 604_000],
    width: 1000,
    busy,
    gutterPx: 24,
  });

  it("collapses the idle stretch into one marked gutter", () => {
    expect(scale.bands).toHaveLength(1);
    expect(scale.bands[0].from).toBe(T0 + 2_000);
    expect(scale.bands[0].to).toBe(T0 + 602_000);
    expect(scale.bands[0].elidedMs).toBe(600_000);
    expect(scale.bands[0].width).toBe(24);
  });

  it("spends the remaining pixels on the two active stretches, equally", () => {
    // 976px for 4000ms of work, split evenly because the two spans are equal.
    expect(scale.x(T0 + 2_000)).toBeCloseTo(488, 5);
    expect(scale.x(T0 + 602_000)).toBeCloseTo(512, 5);
    expect(scale.x(T0 + 604_000)).toBe(1000);
  });

  it("keeps proportions true WITHIN an active stretch", () => {
    const half = scale.x(T0 + 1_000);
    expect(half).toBeCloseTo(244, 5);
  });

  it("inverts across the gutter to a time inside the elided range", () => {
    const middle = scale.invert(500);
    expect(middle).toBeGreaterThan(T0 + 2_000);
    expect(middle).toBeLessThan(T0 + 602_000);
  });

  it("leaves a short gap alone", () => {
    const tight = makeScale({
      domain: [T0, T0 + 10_000],
      width: 1000,
      busy: [
        [T0, T0 + 4_000],
        [T0 + 6_000, T0 + 10_000],
      ],
      idleThresholdMs: 5_000,
    });
    // Two seconds of idle is cheaper to draw than to elide: the gutter costs
    // more pixels than the gap it would replace.
    expect(tight.bands).toEqual([]);
  });
});

describe("the minimum bar width", () => {
  const scale = makeScale({
    domain: [T0, T0 + 600_000],
    width: 1000,
    mode: "linear",
    barMin: 3,
  });

  it("widens a sub-pixel span and says that it did", () => {
    // 3ms out of ten minutes is 0.005px. A bar that is not drawn cannot be
    // hovered, focused or clicked — the step leaves the chart and the keyboard
    // order at the same time.
    const span = scale.span(T0 + 1_000, T0 + 1_003);
    expect(span.width).toBe(3);
    expect(span.clamped).toBe(true);
  });

  it("gives a zero-duration instant the same treatment", () => {
    const span = scale.span(T0 + 1_000, T0 + 1_000);
    expect(span.width).toBe(3);
    expect(span.clamped).toBe(true);
  });

  it("leaves a span that is already wide enough alone", () => {
    const span = scale.span(T0, T0 + 60_000);
    expect(span.clamped).toBe(false);
    expect(span.width).toBeCloseTo(100, 5);
  });

  it("widens leftward at the right edge so nothing spills out of the plot", () => {
    const span = scale.span(T0 + 600_000, T0 + 600_000);
    expect(span.x + span.width).toBeLessThanOrEqual(1000);
  });

  it("tolerates a backwards span", () => {
    const span = scale.span(T0 + 5_000, T0 + 1_000);
    expect(span.width).toBeGreaterThan(0);
    expect(Number.isFinite(span.x)).toBe(true);
  });
});

describe("ticks", () => {
  it("chooses a round duration a reader already thinks in", () => {
    const scale = makeScale({
      domain: [T0, T0 + 600_000],
      width: 1000,
      mode: "linear",
      targetTickPx: 120,
    });
    // 120px of a ten-minute span is 72s; the ladder's next rung up is 2m,
    // because 72s and 75s are not durations anyone has in their head.
    expect(scale.tickStepMs).toBe(120_000);
    expect(scale.ticks.length).toBeGreaterThan(2);
  });

  it("never places a tick inside a gutter", () => {
    const scale = makeScale({
      domain: [T0, T0 + 604_000],
      width: 1000,
      busy: [
        [T0, T0 + 2_000],
        [T0 + 602_000, T0 + 604_000],
      ],
    });
    // A timestamp printed on a zigzag that stands for a RANGE is the one place
    // on this axis where a single instant means nothing. The band's own two
    // edges are exempt: they are also the edges of the active pieces either
    // side, so a tick that lands exactly on one is labelling the work, not the
    // elision.
    for (const tick of scale.ticks) {
      for (const band of scale.bands) {
        expect(tick.t > band.from && tick.t < band.to).toBe(false);
      }
    }
  });

  it("lands every tick inside the plot", () => {
    const scale = makeScale({ domain: [T0, T0 + 45_000], width: 800 });
    for (const tick of scale.ticks) {
      expect(tick.x).toBeGreaterThanOrEqual(0);
      expect(tick.x).toBeLessThanOrEqual(800);
    }
  });
});

describe("degenerate inputs, which are all real", () => {
  it("survives a zero width, which is what the first render before layout has", () => {
    const scale = makeScale({ domain: [T0, T0 + 1_000], width: 0 });
    expect(Number.isFinite(scale.x(T0 + 500))).toBe(true);
    expect(scale.ticks).toEqual([]);
  });

  it("survives a zero-duration domain", () => {
    const scale = makeScale({ domain: [T0, T0], width: 500 });
    expect(Number.isFinite(scale.x(T0))).toBe(true);
    expect(Number.isFinite(scale.invert(250))).toBe(true);
  });

  it("survives an inverted domain", () => {
    const scale = makeScale({ domain: [T0 + 1_000, T0], width: 500 });
    expect(scale.domain[1]).toBeGreaterThan(scale.domain[0]);
  });

  it("stays linear when nothing was ever busy", () => {
    // Every step cancelled before it started. Collapsing the whole domain into
    // one gutter would leave no pixels for the terminal ticks, which are the
    // only thing this chart has left to draw.
    const scale = makeScale({ domain: [T0, T0 + 600_000], width: 1000, busy: [] });
    expect(scale.bands).toEqual([]);
    expect(scale.x(T0 + 300_000)).toBeCloseTo(500, 5);
  });

  it("shrinks the gutters rather than starving the active pieces", () => {
    // Fifty short bursts separated by fifty long idles asks for 1200px of
    // gutter in a 600px chart. Letting the active pieces take a negative width
    // would map every bar in the job to the same x.
    const busy: Array<[number, number]> = [];
    for (let i = 0; i < 50; i += 1) {
      busy.push([T0 + i * 600_000, T0 + i * 600_000 + 100]);
    }
    const scale = makeScale({
      domain: [T0, T0 + 50 * 600_000],
      width: 600,
      busy,
      gutterPx: 24,
    });
    const gutterTotal = scale.bands.reduce((sum, band) => sum + band.width, 0);
    expect(gutterTotal).toBeLessThanOrEqual(600 * 0.6 + 0.001);
    expect(scale.x(T0 + 50 * 600_000)).toBe(600);
    for (const band of scale.bands) expect(band.x).toBeGreaterThanOrEqual(0);
  });

  it("never returns NaN for a non-finite input", () => {
    const scale = makeScale({ domain: [T0, T0 + 1_000], width: 500 });
    expect(scale.x(Number.NaN)).toBe(0);
    expect(scale.invert(Number.NaN)).toBe(T0);
  });
});

describe("the case this scale exists for", () => {
  // A 3ms echo, a 40-minute backoff, and a 500ms retry, in one 1400px viewport.
  const busy: Array<[number, number]> = [
    [T0 + 1_000, T0 + 1_003],
    [T0 + 2_401_000, T0 + 2_401_500],
  ];
  const scale = makeScale({
    domain: [T0, T0 + 2_402_000],
    width: 1400,
    busy,
    gutterPx: 24,
  });

  it("keeps the 3ms step visible and hit-testable", () => {
    const span = scale.span(T0 + 1_000, T0 + 1_003);
    expect(span.width).toBeGreaterThanOrEqual(3);
    expect(span.clamped).toBe(true);
  });

  it("gives the forty minutes a fixed, labelled allowance instead of the page", () => {
    const fortyMinutes = scale.bands.find((band) => band.elidedMs > 2_000_000);
    expect(fortyMinutes).toBeDefined();
    expect(fortyMinutes!.width).toBe(24);
  });

  it("leaves the 500ms retry more than a hundred times the ink of the 3ms step", () => {
    const short = scale.x(T0 + 1_003) - scale.x(T0 + 1_000);
    const long = scale.x(T0 + 2_401_500) - scale.x(T0 + 2_401_000);
    // On a linear scale both would be sub-pixel and indistinguishable.
    expect(long / short).toBeCloseTo(500 / 3, 5);
    expect(long).toBeGreaterThan(200);
  });
});
