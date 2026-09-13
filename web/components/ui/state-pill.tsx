import { cn } from "@/lib/cn";
import {
  JOB_STATES,
  STATE_LABEL,
  STATE_TONE,
  pillClass,
  type JobState,
  type StepState,
} from "@/lib/state";
import { StateGlyph } from "./state-glyph";

type Size = "sm" | "md";

const sizes: Record<Size, string> = {
  sm: "px-2 py-0 text-[0.55rem] gap-1",
  md: "",
};

/**
 * The status badge. Three carriers, never one.
 *
 * Every pill says the WORD, carries its own GLYPH, and wears its own STROKE
 * treatment — dotted for QUEUED, dashed for SCHEDULED, solid for the rest, a
 * heavy left cap on FAILED, a strike-through on CANCELLED. Colour is the
 * fourth carrier and the only one a reader might not have.
 *
 * The stroke treatments are the part most likely to be "tidied" by a later
 * hand, so the reason is written down here: QUEUED (6.60:1) and FAILED
 * (6.50:1) sit at effectively the same relative luminance and are identical
 * under full achromatopsia. Dotted-hollow against solid-capped is the only
 * thing separating them for that reader. Uniforming the strokes would make
 * this palette worse in a way that no screenshot would ever reveal.
 *
 * RUNNING is the single filled pill in the system — bone on ink, the brightest
 * thing on the canvas. That is not emphasis for its own sake: "what is running
 * right now" is the question this dashboard exists to answer, and a filled
 * shape is findable in peripheral vision where a coloured outline is not.
 */
export function StatePill({
  state,
  scope = "step",
  cancelling = false,
  size = "md",
  className,
}: {
  state: StepState | JobState;
  /**
   * Only affects a development-time invariant. A job takes six of the eight
   * values — jobEdges never produces SCHEDULED or RETRYING — so a job pill
   * showing either means the caller mixed up a step and its job, which is
   * otherwise a silent and very confusing bug.
   */
  scope?: "step" | "job";
  /**
   * Derived by the CALLER as `cancel_requested_at != null && !terminal(state)`.
   *
   * There is no CANCELLING state constant in the domain, and deliberately so:
   * cancelJob answers 202 and leaves SCHEDULED and RUNNING steps to drain
   * through their own heartbeat, so the job honestly remains RUNNING while it
   * winds down. Inventing a ninth state here to paper over that would put the
   * UI and the runtime into permanent disagreement.
   */
  cancelling?: boolean;
  size?: Size;
  className?: string;
}) {
  if (process.env.NODE_ENV !== "production" && scope === "job") {
    if (!(JOB_STATES as readonly string[]).includes(state)) {
      console.warn(
        `StatePill: "${state}" is a step state and can never appear on a job. ` +
          `Job rollup produces only ${JOB_STATES.join(", ")}.`,
      );
    }
  }

  const step = state as StepState;

  if (cancelling) {
    return (
      <span
        className={cn(
          pillClass,
          sizes[size],
          // Dashed and amber-adjacent, because cancelling is an in-flight
          // transition and not an outcome. It borrows RETRYING's colour rather
          // than CANCELLED's grey precisely because the job is still doing
          // work: showing it as the settled grey would tell an operator the
          // steps had stopped when they have not.
          "border-dashed border-st-retrying/55 text-st-retrying",
          className,
        )}
      >
        <StateGlyph state="RETRYING" />
        CANCELLING
      </span>
    );
  }

  return (
    <span className={cn(pillClass, sizes[size], STATE_TONE[step], className)}>
      <StateGlyph state={step} />
      {STATE_LABEL[step]}
    </span>
  );
}
