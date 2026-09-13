import type { Detail } from "./types";

/**
 * Turning a validation problem into a place in the editor.
 *
 * `Plan.Validate` collects EVERY problem rather than stopping at the first, and
 * it does that explicitly because the plan's author may be a language model
 * rather than a person — one round trip should return the whole list so the
 * model can fix it in one more. That decision only pays off if the reader can
 * act on the whole list too, and a flat list of forty strings reading
 * `steps[2].depends_on[0]: unknown step "fetch"` is not actionable: the reader
 * has to count array elements in a JSON blob by eye.
 *
 * So there are two pure functions here. One parses the field path the Go side
 * emits; the other finds the line each step starts on in the editor's text.
 * Both are testable, and both fail SOFT — an unparseable path still renders as
 * text, and a step whose line cannot be found simply is not jumpable. A
 * problem list that threw on an unfamiliar path shape would hide the problems.
 */

export interface ProblemAnchor {
  /** The path verbatim, e.g. "steps[2].depends_on[0]" or "name". */
  field: string;
  issue: string;
  /**
   * The index into the plan's `steps` array, when the path names one. Undefined
   * for a job-level problem like "name" or "on_step_failure", which belong to
   * the plan and not to any step.
   */
  stepIndex?: number;
  /** What inside the step: "depends_on", "tool", "params". Undefined when the
   *  problem is with the step as a whole. */
  member?: string;
}

/**
 * Parse one `runmesh.Detail` into something a UI can point at.
 *
 * The grammar is narrow on purpose. The Go side builds these paths with
 * `fmt.Sprintf("steps[%d].%s", i, field)` and nothing else, so matching exactly
 * that shape and falling through to "no anchor" for anything else is honest:
 * a looser regex would confidently mis-anchor a path it did not actually
 * understand, and a problem highlighted on the wrong line is worse than a
 * problem highlighted on none.
 */
export function anchorDetail(detail: Detail): ProblemAnchor {
  const match = /^steps\[(\d+)\](?:\.([A-Za-z_][A-Za-z0-9_]*))?/.exec(detail.field);
  if (!match) return { field: detail.field, issue: detail.issue };

  return {
    field: detail.field,
    issue: detail.issue,
    stepIndex: Number(match[1]),
    member: match[2],
  };
}

export function anchorDetails(details: readonly Detail[]): ProblemAnchor[] {
  return details.map(anchorDetail);
}

/**
 * The 1-based line on which each element of the top-level `steps` array begins.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS IS A SCANNER AND NOT `JSON.parse`.
 *
 * `JSON.parse` throws away every byte of position information, so it can tell
 * you that `steps[2]` exists and never where it is written. The alternatives
 * were a JSON source-map parser (a dependency, for one editor affordance) or a
 * regex for `"id"` (wrong the moment a step's params contain the word).
 *
 * So this walks the text once, tracking string state and escapes so a brace
 * inside a string literal cannot move the depth counter — which is the entire
 * difficulty, and the reason `{"id":"a","params":{"note":"} }"}` does not
 * break it. It deliberately does NOT validate: the text in the editor is very
 * often mid-edit and invalid, which is exactly when a reader most wants the
 * problem list to point somewhere, so an unbalanced document returns the lines
 * it managed to find rather than nothing.
 * ---------------------------------------------------------------------------
 */
export function stepLines(source: string): number[] {
  const lines: number[] = [];

  let line = 1;
  let inString = false;
  let escaped = false;
  /** Nesting depth of {} and [] combined, counted from the document root. */
  let depth = 0;
  /** The depth at which the steps array's ELEMENTS live, once found. */
  let elementDepth = -1;
  /** Set when the scanner has just read the key "steps" and is waiting for its
   *  value to open. */
  let awaitingStepsArray = false;
  let key = "";
  let readingKey = false;

  for (let i = 0; i < source.length; i += 1) {
    const ch = source[i];

    if (ch === "\n") {
      line += 1;
      continue;
    }

    if (inString) {
      if (escaped) escaped = false;
      else if (ch === "\\") escaped = true;
      else if (ch === '"') {
        inString = false;
        if (readingKey) readingKey = false;
      } else if (readingKey) key += ch;
      continue;
    }

    if (ch === '"') {
      inString = true;
      // A string at the top level of an object is a candidate key. It may turn
      // out to be a value; the ":" check below is what decides.
      readingKey = true;
      key = "";
      // `"steps": "not an array"` is malformed but perfectly typeable, and a
      // scanner still armed after it would capture the next array in the
      // document — most likely some step's depends_on — and report its
      // elements as steps.
      awaitingStepsArray = false;
      continue;
    }

    if (ch === ":") {
      // `steps` is only the steps array when it is a key of the ROOT object,
      // which is depth 1. A nested object with its own "steps" member — a
      // tool's params, most plausibly — must not capture the scanner.
      if (key === "steps" && depth === 1) awaitingStepsArray = true;
      key = "";
      continue;
    }

    if (ch === "{" || ch === "[") {
      if (awaitingStepsArray && ch === "[") {
        elementDepth = depth + 1;
      } else if (elementDepth !== -1 && depth === elementDepth && ch === "{") {
        // An object opening at exactly the array's element depth is one step.
        // The `elementDepth !== -1` guard is not redundant: a document with
        // more closing braces than opening ones drives `depth` negative, and
        // -1 === -1 would then start reporting every object as a step.
        lines.push(line);
      }
      awaitingStepsArray = false;
      depth += 1;
      key = "";
      continue;
    }

    if (ch === "}" || ch === "]") {
      awaitingStepsArray = false;
      depth -= 1;
      if (elementDepth !== -1 && depth < elementDepth) {
        // The steps array closed. Anything after it is not a step, and a
        // second "steps" key deeper in the document must not append to this
        // list.
        elementDepth = -1;
      }
      key = "";
      continue;
    }

    if (ch === ",") key = "";
  }

  return lines;
}

/**
 * The 1-based line a problem points at, or null when it cannot be located.
 *
 * A plan-level problem ("name", "on_step_failure") anchors to line 1 rather
 * than to nothing, because scrolling a reader to the top of the document is a
 * correct answer for a problem with the document.
 */
export function lineOfAnchor(anchor: ProblemAnchor, source: string): number | null {
  if (anchor.stepIndex === undefined) return anchor.field ? 1 : null;
  const lines = stepLines(source);
  const line = lines[anchor.stepIndex];
  return line ?? null;
}

/**
 * Group problems by step so the list reads as "step 2 has three problems"
 * rather than as three unrelated sentences that happen to share a prefix.
 *
 * Insertion order is preserved and plan-level problems come first under the
 * key `null`, because a plan whose `name` is empty and whose step 4 names an
 * unknown tool has one problem that invalidates the submission and one that
 * invalidates a step, and the reader should meet them in that order.
 */
export function groupByStep(anchors: readonly ProblemAnchor[]): Array<{
  stepIndex: number | null;
  problems: ProblemAnchor[];
}> {
  const groups = new Map<number | null, ProblemAnchor[]>();
  for (const anchor of anchors) {
    const key = anchor.stepIndex ?? null;
    const bucket = groups.get(key);
    if (bucket) bucket.push(anchor);
    else groups.set(key, [anchor]);
  }

  const out = [...groups.entries()].map(([stepIndex, problems]) => ({ stepIndex, problems }));
  out.sort((a, b) => {
    if (a.stepIndex === null) return -1;
    if (b.stepIndex === null) return 1;
    return a.stepIndex - b.stepIndex;
  });
  return out;
}
