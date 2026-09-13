/**
 * What the chart considers "the thing you are asking about".
 *
 * Its own module, with no React and no DOM, because it is the contract between
 * the chart and whatever hosts it: the job page keeps this in the URL as
 * `?step=&attempt=` so a selection is shareable, and the step inspector reads
 * it. A type that three modules agree on belongs to none of them.
 */
export interface WaterfallSelection {
  stepId: string;
  /**
   * Which attempt of that step, or null for the lane as a whole.
   *
   * Null is a real selection rather than a missing one. "What was this step
   * waiting for" is a question about the lane and not about any one execution
   * of it, and a lane whose bars are clamped to three pixels needs a way to be
   * selected that is not a three-pixel target.
   *
   * It is the domain's `Attempt` — monotonic, incremented on every claim — and
   * NOT an index into the attempts array. Two attempts can share the number
   * when a cancel closes a step that was already closed as RETRYING, so a host
   * round-tripping this through a URL must expect to resolve it against the
   * lane rather than to index with it.
   */
  attempt: number | null;
}
