"use client";

import { viewResult } from "@/lib/result";
import { formatBytes } from "@/lib/tools-policy";
import { Code } from "@/components/ui/code";
import { CopyButton } from "@/components/ui/copy-button";

/**
 * A step's result.
 *
 * ---------------------------------------------------------------------------
 * THE MARKDOWN IS RENDERED AS TEXT. THIS IS A SECURITY DECISION, NOT A STYLE
 * ONE.
 *
 * `report_generate` returns `{markdown, bytes, sections, missing}`, and the
 * obvious improvement to this panel is to render that field as formatted
 * Markdown so a report looks like a report. report.go says, in its own words,
 * that its cell escaping is FORMATTING HYGIENE AND NOT A SECURITY CONTROL —
 * it escapes pipes so tables do not break, and makes no claim about anything
 * else. The content is the results of the steps the report depends on, which
 * in any plan containing http_request includes a fetched web page.
 *
 * So rendering it as HTML would take attacker-controlled bytes, pass them
 * through a pipeline that explicitly disclaims sanitising them, and inject the
 * output into an operator console holding a session against the runtime. The
 * Go side never claimed to close that path and this screen must not open it.
 * ---------------------------------------------------------------------------
 *
 * The classification is a duck check on the shape (`lib/result.ts`), not on
 * the tool's name, because the same renderer is implemented by cmd/task for
 * the container path and a tool name is configuration.
 */
export function ResultView({ result }: { result: unknown }) {
  const view = viewResult(result);

  if (view.kind === "empty") {
    return (
      <p className="text-[0.78rem] text-faint">
        This step returned no result. A tool that succeeds without producing
        output is legitimate — echo returns one, sleep does not.
      </p>
    );
  }

  if (view.kind === "markdown") {
    return (
      <div>
        <div className="flex flex-wrap items-center gap-3">
          <span className="kicker text-[0.5625rem]">markdown report</span>
          {view.bytes !== undefined && (
            <span className="tnum text-[0.62rem] text-faint">{formatBytes(view.bytes)}</span>
          )}
          {view.sections !== undefined && (
            <span className="tnum text-[0.62rem] text-faint">
              {view.sections} {view.sections === 1 ? "section" : "sections"}
            </span>
          )}
          <CopyButton className="ml-auto" value={view.text} label="copy markdown" />
        </div>

        {view.missing && view.missing.length > 0 && (
          // The renderer reports the step ids it was asked to include and could
          // not find. Hiding that would make a report look complete when a
          // dependency's output is simply absent from it.
          <p className="mt-2 text-[0.68rem] text-st-retrying">
            Not included, because these steps produced nothing to present:{" "}
            <span className="tnum">{view.missing.join(", ")}</span>
          </p>
        )}

        <Code className="mt-3" label="Report markdown, shown as text" value={view.text} />
        <p className="mt-2 text-[0.65rem] leading-snug text-faint">
          Shown as text on purpose. This Markdown can contain the output of a
          fetched web page, and the renderer that produced it escapes for
          formatting rather than for safety.
        </p>
      </div>
    );
  }

  return (
    <div>
      <div className="flex items-center gap-3">
        <span className="kicker text-[0.5625rem]">result</span>
        <CopyButton className="ml-auto" value={view.text} label="copy json" />
      </div>
      <Code className="mt-3" label="Step result as JSON" value={view.text} />
    </div>
  );
}
