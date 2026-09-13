"use client";

import { cn } from "@/lib/cn";
import { JOB_STATES, STATE_LABEL, type JobState } from "@/lib/state";
import { StateGlyph } from "@/components/ui/state-glyph";

/**
 * The six job states as toggles.
 *
 * SIX, not eight. `jobEdges` (state.go:119-122) only ever rolls a job up to
 * QUEUED, RUNNING, SUCCEEDED, FAILED, CANCELLED or TIMED_OUT — SCHEDULED is
 * explicitly reserved with no writer and RETRYING is never rolled up. A filter
 * bar carrying all eight would offer two chips that can never match anything,
 * and a reader who clicked one and got an empty list would reasonably conclude
 * the filter was broken rather than that the state was unreachable.
 *
 * The chips are checkboxes semantically — several can be on at once and the
 * API's `?state=` is repeatable — so they are `aria-pressed` toggle buttons
 * rather than radio-styled links. Each carries its glyph as well as its word,
 * for the same reason every pill does.
 *
 * The selected chip is tinted with the state's OWN colour rather than with the
 * accent. That is the one place this product lets a state colour into chrome,
 * and it earns it: the bar is a legend as much as a control, and a reader who
 * learns "amber means retrying" here reads the table faster.
 */
export function StateFilterBar({
  value,
  onToggle,
  onClear,
  className,
}: {
  value: readonly JobState[];
  onToggle: (state: JobState) => void;
  onClear: () => void;
  className?: string;
}) {
  const active = new Set(value);

  return (
    <div className={cn("flex flex-wrap items-center gap-2", className)}>
      <span className="kicker mr-1 text-[0.5625rem]">state</span>

      {JOB_STATES.map((state) => {
        const on = active.has(state);
        return (
          <button
            key={state}
            type="button"
            aria-pressed={on}
            onClick={() => onToggle(state)}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5",
              "font-mono text-[0.6rem] uppercase tracking-[0.14em] transition-colors",
              on
                ? "border-current text-[color:var(--chip)]"
                : "border-line-2 text-muted hover:border-bone/40 hover:text-bone-2",
            )}
            style={{
              transitionDuration: "var(--dur-hover)",
              // Cast-free: a custom property in a style object is the house
              // pattern, and it is how a chip takes its own state's colour
              // without eight hardcoded class strings.
              ["--chip" as string]: `var(--st-${state.toLowerCase().replace("_", "")})`,
            }}
          >
            <StateGlyph state={state} />
            {STATE_LABEL[state]}
          </button>
        );
      })}

      {value.length > 0 && (
        <button
          type="button"
          onClick={onClear}
          className="ml-1 text-[0.68rem] text-faint underline-offset-4 hover:text-bone hover:underline"
        >
          clear
        </button>
      )}
    </div>
  );
}
