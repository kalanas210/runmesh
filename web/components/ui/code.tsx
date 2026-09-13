import { cn } from "@/lib/cn";

/**
 * A block of text that is data: a tool's result, a plan, a schema, a report.
 *
 * ---------------------------------------------------------------------------
 * IT IS NEVER SYNTAX-HIGHLIGHTED AND IT IS NEVER `dangerouslySetInnerHTML`.
 *
 * Highlighting means either a library (a large dependency for a console that
 * ships none) or a regex over the text, and every regex highlighter works the
 * same way: it wraps matches in markup and injects the result as HTML. On this
 * screen the text being wrapped is a step's `result`, which is untyped tool
 * output — and under a plan containing http_request it is a fetched web page.
 * So a highlighter here is an HTML injection path, sitting in an operator
 * console that holds a session against the runtime, in exchange for coloured
 * punctuation.
 *
 * `{value}` as a React child is escaped by React, which is the whole defence
 * and costs nothing.
 * ---------------------------------------------------------------------------
 *
 * `maxHeight` scrolls rather than truncates. A result clipped with an ellipsis
 * is a result whose interesting half might be missing with no indication that
 * it was; a scroll region is honest about there being more.
 */
export function Code({
  value,
  maxHeight = 320,
  label,
  className,
}: {
  value: string;
  /** Pixels. The block scrolls past it; it never clips. */
  maxHeight?: number;
  /** Announced to assistive technology, since a <pre> of JSON is otherwise an
   *  unlabelled wall of text. */
  label?: string;
  className?: string;
}) {
  return (
    <pre
      aria-label={label}
      // Focusable so the scroll region is reachable by keyboard. A scrollable
      // box that only a mouse can reach is a section of the page a keyboard
      // user cannot read at all.
      tabIndex={0}
      className={cn(
        "overflow-auto rounded-xl border border-line bg-ink px-3.5 py-3",
        "font-mono text-[0.72rem] leading-relaxed text-bone-2",
        // Long lines wrap rather than scrolling horizontally: a result is prose
        // and JSON, not a table, and a horizontal scrollbar on every block
        // would hide the ends of lines nobody thought to drag towards.
        "whitespace-pre-wrap break-words",
        className,
      )}
      style={{ maxHeight }}
    >
      {value}
    </pre>
  );
}
