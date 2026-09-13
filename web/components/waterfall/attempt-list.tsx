"use client";

import { cn } from "@/lib/cn";
import { formatDuration, formatOffset } from "@/lib/format";
import { useMediaQuery } from "@/hooks/useMediaQuery";
import { STATE_FILL_VAR, STATE_VAR } from "@/lib/state";
import { StatePill } from "@/components/ui/state-pill";
import { Kicker } from "@/components/ui/kicker";
import { surface } from "@/components/ui/card";
import type { JobResponse } from "@/lib/types";
import { spanTotals, type DepthBand, type Lane, type Segment } from "@/lib/waterfall";

/**
 * The waterfall below 900px: a card per step, not a squeezed chart.
 *
 * A chart whose X axis is time needs horizontal room in proportion to how much
 * happened, and a phone has none. The usual answers are both bad: scrolling the
 * plot sideways hides the dependency structure, which is the thing a reader
 * came for, and scaling it down produces a column of 2px marks that says
 * nothing. So below 900px this is a COMPLETE ALTERNATIVE rendering of the same
 * lanes, built from the same segments, and it answers the same questions in the
 * order a small screen can carry them: what is this step, what state is it in,
 * where did its time go, and why has it not started.
 *
 * The mini bar keeps proportion WITHIN a step and abandons it between steps —
 * every card's bar is 100% wide whatever the step's duration — because the
 * useful comparison on a phone is "where did this step's time go", and the
 * cross-step comparison the chart exists for cannot be made honestly at this
 * width anyway. The absolute duration is printed beside it so the abandoned
 * axis is never mistaken for a real one.
 *
 * Times are relative to the job's creation here, and only here. An absolute
 * UTC stamp is 24 characters and does not fit; a relative one at least tells a
 * reader the order and the gaps.
 */
export function AttemptList({
  job,
  bands,
  segmentsByLane,
  domain,
  selected,
  onSelect,
  className,
}: {
  job: JobResponse;
  bands: readonly DepthBand[];
  segmentsByLane: ReadonlyMap<string, readonly Segment[]>;
  /** The chart's X domain, so "+00:12.4" counts from the same zero the desktop
   *  axis does. */
  domain: [number, number];
  selected: string | null;
  onSelect: (stepId: string | null) => void;
  className?: string;
}) {
  // Below 640px the band headers go and the depth moves onto each card. A
  // header costs a whole row of vertical space on a phone, and at that width a
  // reader is scrolling one card at a time rather than comparing a band.
  const bandHeaders = useMediaQuery("(min-width: 640px)");

  return (
    <div className={cn("space-y-2", className)}>
      {bands.map((band) => (
        <section key={band.depth} className="space-y-2">
          {bandHeaders && (
            <Kicker as="h3" className="flex items-center gap-2 pt-1 text-[0.5625rem]">
              <span>
                depth {band.depth} · {band.lanes.length}{" "}
                {band.lanes.length === 1 ? "step" : "steps"}
              </span>
              <span aria-hidden className="hairline h-px flex-1" />
            </Kicker>
          )}

          {band.lanes.map((lane) => (
            <LaneCard
              key={lane.stepId}
              job={job}
              lane={lane}
              segments={segmentsByLane.get(lane.stepId) ?? []}
              domain={domain}
              showDepth={!bandHeaders}
              selected={selected === lane.stepId}
              onSelect={onSelect}
            />
          ))}
        </section>
      ))}
    </div>
  );
}

function LaneCard({
  job,
  lane,
  segments,
  domain,
  showDepth,
  selected,
  onSelect,
}: {
  job: JobResponse;
  lane: Lane;
  segments: readonly Segment[];
  domain: [number, number];
  showDepth: boolean;
  selected: boolean;
  onSelect: (stepId: string | null) => void;
}) {
  const totals = spanTotals(segments);
  const work = segments.filter(
    (segment) => segment.kind === "pending" || segment.kind === "run",
  );
  const first = work[0] ?? segments[0];
  const last = work[work.length - 1] ?? segments[segments.length - 1];

  // The proportion bar is over queued + pending + ran, in that order, which is
  // the order they happened. Backoff is excluded on purpose: a 40-minute wait
  // between two 3ms attempts would take the entire bar and leave the two
  // executions invisible, which is the same failure the desktop chart solves
  // with the collapsed gutter and cannot be solved at this width.
  const total = totals.queuedMs + totals.pendingMs + totals.runMs;
  const share = (ms: number) => (total > 0 ? (ms / total) * 100 : 0);

  // The error of the LAST attempt, which is the lane's current error. A lane
  // that failed, was retried and then succeeded has none, and announcing the
  // failure it recovered from would send a reader after a fixed problem.
  const error = lane.attempts[lane.attempts.length - 1]?.error;

  return (
    <button
      type="button"
      aria-pressed={selected}
      onClick={() => onSelect(selected ? null : lane.stepId)}
      className={cn(
        surface,
        "block w-full p-4 text-left transition-colors",
        selected && "border-accent/45 bg-ink-3",
      )}
      style={{ transitionDuration: "var(--dur-hover)" }}
    >
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="tnum truncate text-[0.8rem] text-bone">{lane.stepId}</div>
          <div className="mt-0.5 text-[0.65rem] text-faint">
            {lane.tool}
            {showDepth && <span className="kicker ml-2 text-[0.5rem]">depth {lane.depth}</span>}
          </div>
        </div>
        <StatePill state={lane.state} size="sm" />
      </div>

      <div
        className="mt-3 flex h-2.5 w-full overflow-hidden rounded-full border border-line"
        role="presentation"
      >
        {/* The same three textures the chart uses, so a reader who has seen one
            rendering recognises the other. */}
        <span
          className="dotline h-full"
          style={{
            width: `${share(totals.queuedMs)}%`,
            ["--hatch" as string]: `var(${STATE_VAR.QUEUED})`,
          }}
        />
        <span
          className="hatch h-full"
          style={{
            width: `${share(totals.pendingMs)}%`,
            ["--hatch" as string]: `var(${STATE_VAR.SCHEDULED})`,
          }}
        />
        <span
          className="h-full"
          style={{
            width: `${share(totals.runMs)}%`,
            background: `var(${STATE_FILL_VAR[lane.state]})`,
            borderRight: `1px solid var(${STATE_VAR[lane.state]})`,
          }}
        />
      </div>

      <dl className="tnum mt-2 flex flex-wrap gap-x-3 gap-y-1 text-[0.6rem] text-muted">
        <Pair
          k="queued"
          v={`${formatDuration(totals.queuedMs)}${totals.inferred ? "*" : ""}`}
        />
        <Pair k="pending" v={formatDuration(totals.pendingMs)} />
        <Pair k="ran" v={formatDuration(totals.runMs)} />
        {totals.backoffMs > 0 && <Pair k="backoff" v={formatDuration(totals.backoffMs)} />}
        {lane.attempts.length > 1 && (
          <Pair k="attempts" v={`${lane.attempts.length}/${lane.maxAttempts}`} />
        )}
      </dl>

      {first && last && (
        <div className="tnum mt-1.5 text-[0.575rem] text-faint">
          {formatOffset(first.from - domain[0])} → {formatOffset(last.to - domain[0])} from job
          start
        </div>
      )}

      {totals.inferred && (
        // The asterisk above has to mean something on the card that carries it.
        // Nothing in the runtime records when a step became eligible, so this
        // number is derived from the job's creation and its dependencies'
        // completion, and it must never be presented as if it were measured.
        <p className="mt-1 text-[0.55rem] leading-snug text-faint">
          * queued time is inferred — the runtime records no per-step eligibility
          timestamp.
        </p>
      )}

      {lane.state === "QUEUED" && lane.blockedBy.length > 0 && (
        <p className="tnum mt-1.5 text-[0.6rem] text-st-queued">
          blocked by {lane.blockedBy.join(", ")}
        </p>
      )}

      {error && (
        <p className="mt-1.5 text-[0.6rem] leading-snug text-st-failed">
          <span className="tnum">{error.code}</span>
          <span className="text-muted"> — {error.message}</span>
        </p>
      )}

      {job.cancel_requested_at && !lane.attempts.length && (
        <p className="mt-1.5 text-[0.6rem] text-st-cancelled">
          cancelled before it was claimed
        </p>
      )}
    </button>
  );
}

function Pair({ k, v }: { k: string; v: string }) {
  return (
    <span className="flex gap-1">
      <dt className="text-faint">{k}</dt>
      <dd className="text-bone-2">{v}</dd>
    </span>
  );
}
