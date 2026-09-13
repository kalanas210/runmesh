"use client";

import { anchorDetails, groupByStep, lineOfAnchor } from "@/lib/plan-problems";
import type { Detail } from "@/lib/types";

/**
 * Every validation problem, grouped by the step it belongs to.
 *
 * ---------------------------------------------------------------------------
 * WHY GROUPING MATTERS MORE HERE THAN IT NORMALLY WOULD.
 *
 * `Plan.Validate` collects EVERY problem rather than stopping at the first, and
 * it does that explicitly because the plan's author may be a language model —
 * one round trip should return the whole list so the model can fix it in one
 * more. That decision only pays off if a human reader can act on the whole list
 * too, and a flat run of forty sentences reading
 * `steps[2].depends_on[0]: unknown step "fetch"` is not actionable: the reader
 * has to count array elements in a JSON blob by eye to find the step it means.
 *
 * So the field path is parsed, the problems are grouped under their step, and
 * each group jumps the editor to the line that step starts on. Plan-level
 * problems come first under no step, because a plan whose `name` is empty and
 * whose step 4 names an unknown tool has one problem that invalidates the
 * submission and one that invalidates a step — and the reader should meet them
 * in that order.
 * ---------------------------------------------------------------------------
 *
 * Anchoring FAILS SOFT throughout. A path this parser does not recognise still
 * renders as text and simply is not jumpable; a problem highlighted on the
 * wrong line would be worse than one highlighted on none.
 */
export function ProblemList({
  details,
  source,
  onJump,
}: {
  details: readonly Detail[];
  /** The editor's current text, which is what the line numbers are into. */
  source: string;
  onJump?: (line: number) => void;
}) {
  if (details.length === 0) return null;

  const groups = groupByStep(anchorDetails(details));

  return (
    <div className="rounded-2xl border border-[#ff6b6b]/40 bg-[#ff6b6b]/[0.04] p-5">
      <h3 className="text-[0.82rem] text-[#ff6b6b]">
        {details.length} {details.length === 1 ? "problem" : "problems"} with this plan
      </h3>
      <p className="mt-1 text-[0.68rem] text-faint">
        The runtime reports every problem at once rather than the first, so this
        is the complete list.
      </p>

      <ul className="mt-4 space-y-4">
        {groups.map((group) => {
          const line =
            group.problems.length > 0 ? lineOfAnchor(group.problems[0], source) : null;

          return (
            <li key={group.stepIndex ?? "plan"}>
              <div className="flex items-baseline gap-2">
                <span className="kicker text-[0.5rem]">
                  {group.stepIndex === null ? "the plan" : `step ${group.stepIndex}`}
                </span>
                {line !== null && onJump && (
                  <button
                    type="button"
                    onClick={() => onJump(line)}
                    className="tnum text-[0.62rem] text-muted underline-offset-4 hover:text-bone hover:underline"
                  >
                    line {line}
                  </button>
                )}
              </div>

              <ul className="mt-1.5 space-y-1.5">
                {group.problems.map((problem, index) => (
                  <li key={`${problem.field}-${index}`} className="text-[0.72rem] leading-snug">
                    {/* The path verbatim as well as the sentence. It is what the
                        API said, it is what a caller writing curl would see, and
                        a reader comparing the two should find the same string. */}
                    <span className="tnum text-bone-2">{problem.field}</span>
                    <span className="ml-2 text-muted">{problem.issue}</span>
                  </li>
                ))}
              </ul>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
