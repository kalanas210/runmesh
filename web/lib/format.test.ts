import { describe, expect, it } from "vitest";
import {
  formatAge,
  formatDuration,
  formatInstant,
  formatOffset,
  timeoutToMs,
} from "./format";

/**
 * The duration formatter has to cover an enormous range — a 3ms echo step and a
 * 40-minute retry backoff appear in the same column — and every boundary where
 * the unit changes is a place a number can come out wrong or, worse, come out
 * plausible and wrong. So the boundaries are pinned.
 */

describe("formatDuration", () => {
  const cases: Array<[number | null | undefined, string]> = [
    [0, "0ms"],
    [3, "3ms"],
    [999, "999ms"],
    [1000, "1.00s"],
    [3040, "3.04s"],
    [59_999, "60.00s"],
    [60_000, "1m 00s"],
    [252_000, "4m 12s"],
    [3_599_000, "59m 59s"],
    [3_600_000, "1h 00m"],
    [7_890_000, "2h 11m"],
    // Absent is the em dash, not "0" — a step that never ran and a step that
    // took no time are different facts and must not print the same.
    [null, "—"],
    [undefined, "—"],
    [NaN, "—"],
  ];

  it.each(cases)("formats %p as %p", (ms, want) => {
    expect(formatDuration(ms)).toBe(want);
  });

  it("prints exact milliseconds when precise", () => {
    // The hover card on a bar clamped to the 3px minimum uses this: the chart
    // is necessarily lying about that segment's width, so the number beside it
    // must not round the truth away too.
    expect(formatDuration(3, true)).toBe("3ms");
    expect(formatDuration(252_000, true)).toBe("252000ms");
  });
});

describe("formatAge", () => {
  const cases: Array<[number, string]> = [
    [0, "0s"],
    [999, "0s"],
    [2_000, "2s"],
    [59_000, "59s"],
    [60_000, "1m 00s"],
    [200_000, "3m 20s"],
    [3_600_000, "1h 00m"],
    // A clock skew that puts "last updated" in the future must not print "-3s".
    [-5_000, "0s"],
  ];

  it.each(cases)("formats %p as %p", (ms, want) => {
    expect(formatAge(ms)).toBe(want);
  });
});

describe("formatInstant", () => {
  // The month abbreviation is matched as `Sept?` on purpose. en-GB spells
  // September "Sept" on a current ICU and "Sep" on older ones, and that
  // difference is not what this test is about — pinning one spelling would make
  // the suite fail on a Node built against a different ICU while telling us
  // nothing about the thing that actually matters. The date, the time, the
  // conversion and the suffix are all asserted exactly.
  it("renders in UTC and says so", () => {
    // Pinned to UTC and to en-GB so the server and the browser produce the same
    // string. If this ever drifts to the host's locale or offset, hydration
    // breaks on every screen at once.
    expect(formatInstant("2026-09-12T03:24:11Z")).toMatch(
      /^12 Sept? 2026, 03:24:11 UTC$/,
    );
  });

  it("renders an offset instant in UTC, not in its own offset", () => {
    // 09:24:11+06:00 is 03:24:11Z. A formatter that quietly used the host's
    // zone would print 09:24 here and every step in the waterfall would be six
    // hours out on one developer's machine and correct on everyone else's.
    expect(formatInstant("2026-09-12T09:24:11+06:00")).toMatch(
      /^12 Sept? 2026, 03:24:11 UTC$/,
    );
  });

  it("gives the em dash for absent and unparseable values", () => {
    expect(formatInstant(null)).toBe("—");
    expect(formatInstant(undefined)).toBe("—");
    expect(formatInstant("not a time")).toBe("—");
  });
});

describe("timeoutToMs", () => {
  it("converts the one duration on this API that is in seconds", () => {
    // stepResponse.timeout_seconds is a float in SECONDS while every other
    // duration on the wire is integer milliseconds. This exists so the
    // multiplication happens in exactly one place.
    expect(timeoutToMs(30)).toBe(30_000);
    expect(timeoutToMs(0.5)).toBe(500);
  });
});

describe("formatOffset", () => {
  it("reads as a stopwatch inside the hour, to a tenth", () => {
    expect(formatOffset(0)).toBe("+00:00.0");
    expect(formatOffset(12_400)).toBe("+00:12.4");
    expect(formatOffset(90_050)).toBe("+01:30.0");
  });

  it("drops to whole seconds past the hour, where a tenth is noise", () => {
    expect(formatOffset(3_749_000)).toBe("+1:02:29");
  });

  it("never renders a negative offset, which a skewed worker clock can produce", () => {
    expect(formatOffset(-5_000)).toBe("+00:00.0");
    expect(formatOffset(Number.NaN)).toBe("+00:00.0");
  });
});
