import { cn } from "@/lib/cn";
import { describeEvent } from "@/lib/event-view";
import { formatDuration, formatTimeOfDay } from "@/lib/format";
import type { RunMeshEvent } from "@/lib/types";
import { StatePill } from "@/components/ui/state-pill";

/**
 * One line of the timeline.
 *
 * The words come from `describeEvent`, which switches exhaustively over all
 * fourteen EventType constants and narrows `attrs` per type — in the pure
 * module, where the branches have tests, rather than here where they would be
 * assertions nothing could make. See lib/event-view.ts for why a careless
 * `attrs.owner` is not a crash but the word "undefined" in an operator's
 * timeline.
 *
 * A job-level event is indented differently from a step-level one and carries
 * no step id, because the two are genuinely different scopes: JOB_FINISHED is
 * about the run, STEP_FINISHED is about one row of it, and a flat list that
 * renders both identically makes the reader reconstruct which is which from the
 * presence of a column.
 */
export function EventRow({
  event,
  onSelectStep,
  className,
}: {
  event: RunMeshEvent;
  /** Clicking the step id selects that lane in the chart. Omitted on screens
   *  where there is no chart to select in. */
  onSelectStep?: (stepId: string, attempt: number | null) => void;
  className?: string;
}) {
  const view = describeEvent(event);

  return (
    <li
      className={cn(
        "flex flex-wrap items-baseline gap-x-3 gap-y-1 border-b border-line/60 px-4 py-2 last:border-b-0",
        view.scope === "job" && "bg-bone/[0.02]",
        className,
      )}
    >
      {/* seq before the clock. The clock is what a reader correlates with a log
          line; the seq is what they quote when the history is truncated, since
          "truncated before seq N" is the message they will have just read. */}
      <span className="tnum w-10 shrink-0 text-[0.62rem] text-faint">{event.seq}</span>
      <span className="tnum w-20 shrink-0 text-[0.68rem] text-muted">
        {formatTimeOfDay(event.at)}
      </span>

      <span
        className={cn(
          "shrink-0 text-[0.75rem]",
          view.produced ? "text-bone-2" : "text-st-retrying",
        )}
      >
        {view.title}
      </span>

      {!view.produced && (
        // Four EventType constants are declared with no producer anywhere in
        // the repo. One arriving means something new is writing events, which
        // is more interesting than the event itself.
        <span className="rounded-full border border-st-retrying/40 px-1.5 text-[0.55rem] uppercase tracking-[0.14em] text-st-retrying">
          undocumented
        </span>
      )}

      {event.step_id && (
        <button
          type="button"
          onClick={() => onSelectStep?.(event.step_id!, event.attempt ?? null)}
          disabled={!onSelectStep}
          className={cn(
            "tnum shrink-0 text-[0.68rem] text-muted",
            onSelectStep && "underline decoration-line-2 underline-offset-4 hover:text-bone",
          )}
        >
          {event.step_id}
          {/* `attempt` is omitempty and vanishes at zero, which is every
              job-level event and any step event before the first claim. It is
              printed only when it is a real attempt number. */}
          {event.attempt ? ` #${event.attempt}` : ""}
        </button>
      )}

      {event.state && <StatePill state={event.state} size="sm" className="shrink-0" />}

      {event.duration_ms !== undefined && (
        <span className="tnum shrink-0 text-[0.65rem] text-faint">
          {formatDuration(event.duration_ms)}
        </span>
      )}

      {view.detail && (
        <span className="min-w-0 basis-full pl-[7.5rem] text-[0.68rem] leading-snug text-faint">
          {view.detail}
        </span>
      )}

      {event.error && (
        <span className="min-w-0 basis-full pl-[7.5rem] font-mono text-[0.68rem] leading-snug text-st-failed">
          {event.error.code}: {event.error.message}
        </span>
      )}
    </li>
  );
}
