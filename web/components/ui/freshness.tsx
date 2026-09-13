"use client";

import { cn } from "@/lib/cn";
import { formatAge } from "@/lib/format";
import { useNow } from "@/lib/now";

/**
 * The stale-data chip.
 *
 * This component exists because of one conviction: a dashboard that shows a
 * green "live" dot over a stalled poll is worse than one that polls visibly.
 * The first tells an operator that nothing is happening. The second tells them
 * they cannot currently see what is happening. Those are opposite facts, and
 * the failure mode of a live indicator is to report the wrong one.
 *
 * So the age is always on screen and always counting, and it changes character
 * as it grows rather than only when something breaks:
 *
 *   under 15s   muted     the normal state, a number nobody needs to read
 *   15s to 30s  amber     slower than it should be
 *   over 30s    amber     RECONNECTING — the poll has missed several turns
 *   over 60s    amber     the caller should also dim the content it describes
 *
 * `terminal` replaces the whole thing with FINAL. A finished job's data cannot
 * go stale, so a ticking age on one would be an anxiety generator measuring
 * nothing — and it is also the signal that polling has correctly stopped.
 */

export type FreshnessLevel = "fresh" | "slow" | "reconnecting" | "stale";

const SLOW_MS = 15_000;
const RECONNECTING_MS = 30_000;
const STALE_MS = 60_000;

export function freshnessLevel(ageMs: number): FreshnessLevel {
  if (ageMs >= STALE_MS) return "stale";
  if (ageMs >= RECONNECTING_MS) return "reconnecting";
  if (ageMs >= SLOW_MS) return "slow";
  return "fresh";
}

/**
 * The opacity a content region should take at a given age. Exported rather than
 * applied here, because the chip is in the page header and the thing that must
 * dim is the region below it.
 */
export function stalenessOpacity(ageMs: number): number {
  return freshnessLevel(ageMs) === "stale" ? 0.6 : 1;
}

const base =
  "tnum inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-[0.6rem] uppercase tracking-[0.14em]";

const tones: Record<FreshnessLevel, string> = {
  fresh: "border-line-2 text-faint",
  slow: "border-st-retrying/40 text-st-retrying",
  reconnecting: "border-st-retrying/55 text-st-retrying",
  stale: "border-st-retrying/55 text-st-retrying",
};

export function Freshness({
  updatedAt,
  polling = true,
  error,
  terminal = false,
  className,
}: {
  /** Epoch ms of the last successful response. Null before the first one. */
  updatedAt: number | null;
  polling?: boolean;
  error?: unknown;
  /** The job has reached a terminal state and polling has stopped. */
  terminal?: boolean;
  className?: string;
}) {
  // Null during server render, then ticking once a second off the one shared
  // clock. Reading Date.now() in the render body instead would put a different
  // number in the server's HTML than in the browser's first render, which is a
  // hydration mismatch on a component that appears on every screen.
  const now = useNow();

  if (terminal) {
    return (
      <span className={cn(base, "border-line-2 text-faint", className)}>FINAL</span>
    );
  }

  if (error) {
    return (
      <span
        role="status"
        className={cn(base, "border-[#ff6b6b]/40 text-[#ff6b6b]", className)}
      >
        NOT UPDATING
      </span>
    );
  }

  if (updatedAt === null || now === null) {
    return <span className={cn(base, tones.fresh, className)}>UPDATING…</span>;
  }

  const age = Math.max(0, now - updatedAt);
  const level = freshnessLevel(age);

  return (
    <span role="status" className={cn(base, tones[level], className)}>
      {level === "fresh" || level === "slow"
        ? `UPDATED ${formatAge(age)} AGO`
        : `RECONNECTING · ${formatAge(age)}`}
      {!polling && level === "fresh" && <span className="text-faint">· PAUSED</span>}
    </span>
  );
}
