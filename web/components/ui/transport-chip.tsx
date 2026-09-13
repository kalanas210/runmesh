"use client";

import { cn } from "@/lib/cn";
import type { TransportState } from "@/lib/feed";

/**
 * How this screen is being fed, in one chip, next to the freshness age.
 *
 * ---------------------------------------------------------------------------
 * THE DEGRADATION IS THE WHOLE POINT OF THIS COMPONENT.
 *
 * A console that opens an SSE stream, loses it, and quietly falls back to a
 * one-second poll looks IDENTICAL to one whose stream is healthy: the events
 * still arrive, just later, and the only symptom is a waterfall whose right
 * edge lags in a way nobody can attribute. Every plausible cause of that
 * fallback is a real condition somebody would want to know about — the API
 * build predates the stream route, the key lacks jobs.read, a proxy between
 * here and the runtime buffers text/event-stream — and all three are fixable
 * by the person reading this chip.
 *
 * So the fallback is announced, with its reason, and the reason is a sentence
 * rather than a status code. The cost is one chip. The alternative is a green
 * dot that means nothing.
 * ---------------------------------------------------------------------------
 *
 * FINAL is not a degradation and is not styled as one: a terminal job's stream
 * closed because there will never be another event, and both the stream and the
 * poll stopping is the correct, cheapest outcome.
 */

const base =
  "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 font-mono text-[0.6rem] uppercase tracking-[0.14em]";

export function TransportChip({
  transport,
  className,
}: {
  transport: TransportState;
  className?: string;
}) {
  if (transport.mode === "final") {
    return <span className={cn(base, "border-line-2 text-faint", className)}>stream closed</span>;
  }

  if (transport.mode === "connecting") {
    return <span className={cn(base, "border-line-2 text-faint", className)}>connecting…</span>;
  }

  if (transport.mode === "live") {
    return (
      <span className={cn(base, "border-accent/45 text-accent", className)}>
        {/* The one place a pulse is allowed outside the chart: it means the
            same thing there — something is live — and the global reduced-motion
            block stops it. */}
        <span aria-hidden className="relative inline-flex h-1.5 w-1.5">
          <span
            data-pulse
            className="absolute inline-flex h-full w-full rounded-full bg-accent"
            style={{ animation: "rm-pulse 2s var(--ease-out-expo) infinite" }}
          />
          <span className="relative inline-flex h-1.5 w-1.5 rounded-full bg-accent" />
        </span>
        live
      </span>
    );
  }

  return (
    <span
      role="status"
      className={cn(base, "border-st-retrying/50 text-st-retrying", className)}
      // The sentence is the useful half and it is too long for a chip, so it is
      // the title as well as the visible text below the header. A tooltip is
      // not the only carrier: the job header prints the same reason inline.
      title={transport.reason}
    >
      polling
    </span>
  );
}

/**
 * The reason, as a full sentence, for the line under the page header.
 *
 * Separate from the chip because a chip cannot hold a sentence and a sentence
 * in a tooltip is a sentence most readers never see. Returns null while the
 * stream is carrying the screen, so the chrome disappears when there is nothing
 * to report.
 */
export function transportReason(transport: TransportState): string | null {
  if (transport.mode !== "polling") return null;
  const reason = transport.reason ?? "the event stream is not connected";
  return transport.permanent
    ? `Live updates are unavailable: ${reason}. The console is polling once a second instead.`
    : `Live updates dropped: ${reason}. The console is polling once a second until the stream comes back.`;
}
