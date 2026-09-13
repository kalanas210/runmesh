/**
 * A step's result, classified for rendering — and the security note that is
 * the entire reason this is a module rather than a `<pre>{JSON.stringify(…)}`.
 *
 * ---------------------------------------------------------------------------
 * `result.markdown` IS NOT RENDERED AS HTML, EVER.
 *
 * `report_generate` returns `{markdown, bytes, sections, missing}` and the
 * obvious, attractive thing to do with that field is run it through a Markdown
 * renderer so the report looks like a report. It must not be done here, and
 * the reason is in report.go's own words: its cell escaping is FORMATTING
 * HYGIENE, NOT A SECURITY CONTROL. The renderer escapes pipes so a table does
 * not break, and makes no claim about script tags or javascript: URLs.
 *
 * The content of that Markdown is the results of the steps it depends on,
 * which in a plan that includes http_request is a fetched web page. So
 * rendering it as HTML would take attacker-controlled bytes, pass them through
 * a pipeline that explicitly disclaims sanitising them, and inject them into
 * an operator console that holds a session against the runtime. The Go side
 * never claimed to close that path; a dashboard must not open it on its
 * behalf.
 *
 * Rendering it as TEXT in a monospace block costs the reader nothing they
 * cannot get by copying it out, and it is the only rendering that is safe
 * without a sanitiser this product does not have and should not acquire for
 * one panel.
 * ---------------------------------------------------------------------------
 *
 * The classification is a duck check on `markdown` rather than a check on the
 * tool name. Tool names are configuration — cmd/task implements the same
 * renderer for the container path — and keying the view off the shape of the
 * data means a second tool that returns Markdown gets the same treatment
 * automatically, while a tool that happens to be called report_generate but
 * returns something else is not mis-rendered.
 */

export type ResultView =
  | { kind: "empty" }
  | { kind: "markdown"; text: string; bytes?: number; sections?: number; missing?: string[] }
  | { kind: "json"; text: string };

/** `null` is a legitimate result — a tool that succeeded and returned nothing —
 *  and is not the same as a step that has not run. The caller distinguishes
 *  those two by the step's state; this function only reads the value. */
export function viewResult(result: unknown): ResultView {
  if (result === undefined || result === null) return { kind: "empty" };

  if (typeof result === "object" && !Array.isArray(result)) {
    const record = result as Record<string, unknown>;
    if (typeof record.markdown === "string") {
      return {
        kind: "markdown",
        text: record.markdown,
        bytes: typeof record.bytes === "number" ? record.bytes : undefined,
        sections: typeof record.sections === "number" ? record.sections : undefined,
        missing: Array.isArray(record.missing)
          ? record.missing.filter((value): value is string => typeof value === "string")
          : undefined,
      };
    }
  }

  // Two-space indentation, because this is read by a person and the alternative
  // — one long line — is unreadable at exactly the moment it matters, which is
  // a result that does not look like what the reader expected.
  return { kind: "json", text: safeStringify(result) };
}

/**
 * `JSON.stringify` throws on a circular structure and returns `undefined` for
 * a bare function or symbol. Neither can arrive over JSON today, but this
 * value is typed `unknown` precisely because nothing validates it, and an
 * inspector panel that throws takes the whole job screen with it.
 */
function safeStringify(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? String(value);
  } catch {
    return String(value);
  }
}
