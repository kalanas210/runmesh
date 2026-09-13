/**
 * The state vocabulary, transcribed from internal/runmesh/state.go:35-56.
 *
 * THE HOUSE RULE, and the reason this file exists at all: colour is never the
 * only carrier. Every pill also says the word, carries its own glyph and its
 * own stroke treatment; every waterfall segment also has a texture. The accent
 * is never a state. A reader who cannot distinguish two of these hues must
 * still be able to read the screen, and on a dashboard where colour IS data
 * that is not a nicety — it is whether the product works.
 *
 * Two separable lists, not one. jobEdges (state.go:119-122) only ever produces
 * six values at job level: SCHEDULED is explicitly reserved with no writer and
 * RETRYING is never rolled up. Pointing an eight-value legend at job.state
 * would carry two entries that can never light.
 *
 * "UNKNOWN" is deliberately absent. It is the Go zero value, and ParseState
 * (state.go:71) explicitly REJECTS it as a wire string, so it can never arrive.
 */

export const STEP_STATES = [
  "QUEUED",
  "SCHEDULED",
  "RUNNING",
  "RETRYING",
  "SUCCEEDED",
  "FAILED",
  "CANCELLED",
  "TIMED_OUT",
] as const;

export const JOB_STATES = [
  "QUEUED",
  "RUNNING",
  "SUCCEEDED",
  "FAILED",
  "CANCELLED",
  "TIMED_OUT",
] as const;

export type StepState = (typeof STEP_STATES)[number];
export type JobState = (typeof JOB_STATES)[number];

/** State.Terminal() — state.go:80-88. Absorbing: every transition out is rejected. */
export const TERMINAL: ReadonlySet<StepState> = new Set<StepState>([
  "SUCCEEDED",
  "FAILED",
  "CANCELLED",
  "TIMED_OUT",
]);

/** State.Active() — state.go:90. Exactly the two states in which a worker holds a lease. */
export const ACTIVE: ReadonlySet<StepState> = new Set<StepState>([
  "SCHEDULED",
  "RUNNING",
]);

export function isTerminal(state: StepState): boolean {
  return TERMINAL.has(state);
}

export function isActive(state: StepState): boolean {
  return ACTIVE.has(state);
}

/**
 * The shared badge shape, an exported string rather than a component, so a
 * caller can compose it with a tone through cn() without a wrapper element.
 * Lifted from ApexTick's lib/status.ts:71 and narrowed: px-2.5/py-0.5 instead
 * of px-3/py-1, because these sit inside 28px table rows and lane labels.
 */
export const pillClass =
  "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 font-mono text-[0.6rem] uppercase tracking-[0.14em]";

/**
 * Border + text only, never a filled background — with exactly one exception.
 *
 * RUNNING is the one solid pill in the system. It is bone on ink, the highest
 * contrast pairing available and the brightest thing on the canvas, because
 * "what is running right now" is the question an operator opens this dashboard
 * to answer and it should be findable without reading. It is also the reason
 * the accent could be blue at all: moving RUNNING out of hue space freed the
 * entire 190-230deg arc.
 *
 * The stroke treatments are load-bearing, not decoration. QUEUED is dotted and
 * FAILED is solid with a heavy left cap specifically because those two sit at
 * the same relative luminance (6.60:1 against 6.50:1) and are identical under
 * full achromatopsia. Uniforming these strokes would erase the only cue that
 * separates them for that reader.
 */
export const STATE_TONE: Record<StepState, string> = {
  QUEUED: "border-st-queued/45 border-dotted text-st-queued",
  SCHEDULED: "border-st-scheduled/50 border-dashed text-st-scheduled",
  RUNNING: "border-transparent bg-st-running text-ink",
  RETRYING: "border-st-retrying/55 text-st-retrying",
  SUCCEEDED: "border-st-succeeded/50 text-st-succeeded",
  FAILED: "border-l-2 border-st-failed/55 text-st-failed",
  TIMED_OUT: "border-st-timedout/55 text-st-timedout",
  CANCELLED: "border-st-cancelled/45 text-st-cancelled line-through decoration-1",
};

/** The word, as it is spoken rather than as it is spelled on the wire. */
export const STATE_LABEL: Record<StepState, string> = {
  QUEUED: "QUEUED",
  SCHEDULED: "SCHEDULED",
  RUNNING: "RUNNING",
  RETRYING: "RETRYING",
  SUCCEEDED: "SUCCEEDED",
  FAILED: "FAILED",
  TIMED_OUT: "TIMED OUT",
  CANCELLED: "CANCELLED",
};

export type GlyphName =
  | "ring-hollow"
  | "ring-half"
  | "dot-pulse"
  | "retry"
  | "check"
  | "cross"
  | "clock"
  | "slash";

/**
 * The non-colour shape. Hand-drawn 10x10 SVGs in the house icon style, never
 * emoji: an emoji is a font-dependent colour image that ignores currentColor
 * and renders differently on every platform, which defeats the entire point of
 * having a second carrier.
 */
export const STATE_GLYPH: Record<StepState, GlyphName> = {
  QUEUED: "ring-hollow",
  SCHEDULED: "ring-half",
  RUNNING: "dot-pulse",
  RETRYING: "retry",
  SUCCEEDED: "check",
  FAILED: "cross",
  TIMED_OUT: "clock",
  CANCELLED: "slash",
};

export type StateTexture = "solid" | "hatch" | "dot" | "hollow" | "solid-gap";

/**
 * The third carrier, for the waterfall. A bar's texture says what KIND of
 * interval it is independently of its colour: hatch is "claimed, not yet
 * started" (the pod-pending gap), dot is "eligible but unclaimed", hollow is
 * "never ran", solid is "the tool was executing".
 */
export const STATE_TEXTURE: Record<StepState, StateTexture> = {
  QUEUED: "dot",
  SCHEDULED: "hatch",
  RUNNING: "solid",
  RETRYING: "solid-gap",
  SUCCEEDED: "solid",
  FAILED: "solid",
  TIMED_OUT: "solid",
  CANCELLED: "hollow",
};

/**
 * The CSS custom property holding a state's colour, for the places that need
 * the raw value in an inline style (an SVG stroke, a bar fill) rather than a
 * Tailwind class. Keyed off the same map so the two cannot drift.
 */
export const STATE_VAR: Record<StepState, string> = {
  QUEUED: "--st-queued",
  SCHEDULED: "--st-scheduled",
  RUNNING: "--st-running",
  RETRYING: "--st-retrying",
  SUCCEEDED: "--st-succeeded",
  FAILED: "--st-failed",
  TIMED_OUT: "--st-timedout",
  CANCELLED: "--st-cancelled",
};

export const STATE_FILL_VAR: Record<StepState, string> = {
  QUEUED: "--st-queued-fill",
  SCHEDULED: "--st-scheduled-fill",
  RUNNING: "--st-running-fill",
  RETRYING: "--st-retrying-fill",
  SUCCEEDED: "--st-succeeded-fill",
  FAILED: "--st-failed-fill",
  TIMED_OUT: "--st-timedout-fill",
  CANCELLED: "--st-cancelled-fill",
};

/**
 * A job is "cancelling" when the operator has asked and the job has not yet
 * settled. There is NO CANCELLING state constant in the domain: cancelJob
 * answers 202 and leaves leased steps to drain through their heartbeat, so
 * `state: "RUNNING"` with `cancel_requested_at` set is a correct response, not
 * a race. The affordance is derived here and nowhere else.
 */
export function isCancelling(
  state: StepState | JobState,
  cancelRequestedAt: string | null | undefined,
): boolean {
  return !!cancelRequestedAt && !TERMINAL.has(state as StepState);
}
