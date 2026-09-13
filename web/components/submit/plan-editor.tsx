"use client";

import { useRef } from "react";
import { cn } from "@/lib/cn";
import { CopyButton } from "@/components/ui/copy-button";

/**
 * The plan, editable.
 *
 * A textarea and not a JSON editor component. `params` is untyped raw JSON per
 * tool — its schema is whatever the tool published — so a generated form cannot
 * represent it, and a syntax-highlighting editor is a dependency plus an HTML
 * injection surface (every regex highlighter injects markup) for coloured
 * punctuation on a screen that is used for thirty seconds at a time.
 *
 * What the reviewer actually needs is the ability to change a value and submit
 * what they are looking at, which is exactly what a textarea gives.
 *
 * The one affordance beyond plain text is `focusLine`, which the problem list
 * calls: the Go side's field paths are literally `steps[2].depends_on[0]`, a
 * scanner in lib/plan-problems.ts turns the index into a line, and this puts
 * the caret there. Selecting the whole line rather than only placing the caret
 * is deliberate — a caret at column 0 of line 14 is invisible in a hundred-line
 * document, and the reader is looking for where they were sent.
 */
export function PlanEditor({
  value,
  onChange,
  invalid = false,
  label = "Plan JSON",
  rows = 20,
  editorRef,
}: {
  value: string;
  onChange: (value: string) => void;
  /** The draft does not parse. Draws the invalid modifier, and nothing else —
   *  the message belongs beside the editor, not inside it. */
  invalid?: boolean;
  label?: string;
  rows?: number;
  /** Handed back so a caller can jump to a line. */
  editorRef?: React.RefObject<HTMLTextAreaElement | null>;
}) {
  const own = useRef<HTMLTextAreaElement>(null);
  const ref = editorRef ?? own;

  return (
    <div>
      <div className="flex items-center gap-3">
        <label htmlFor="plan-editor" className="kicker text-[0.5625rem]">
          {label}
        </label>
        <CopyButton className="ml-auto" value={value} label="copy plan" />
      </div>

      <textarea
        id="plan-editor"
        ref={ref}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        rows={rows}
        spellCheck={false}
        // The browser's autocorrect on a JSON document turns quotes into smart
        // quotes, which produces a parse error whose cause is invisible.
        autoCorrect="off"
        autoCapitalize="off"
        className={cn(
          "mt-2 block w-full resize-y rounded-xl border bg-ink px-3.5 py-3",
          "font-mono text-[0.75rem] leading-relaxed text-bone-2",
          "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
          invalid ? "border-[#ff6b6b]/60" : "border-line",
        )}
      />
    </div>
  );
}

/**
 * Put the caret on a 1-based line and select it.
 *
 * Exported as a function over the element rather than as an imperative handle,
 * because the only caller already holds the ref and a handle would be an extra
 * indirection over two lines of arithmetic.
 */
export function focusLine(element: HTMLTextAreaElement | null, line: number): void {
  if (!element) return;

  const lines = element.value.split("\n");
  // Clamped rather than trusted: the line came from a scanner over text the
  // reader may have edited since the problems arrived, so it can point past the
  // end of the document.
  const index = Math.min(Math.max(line, 1), lines.length) - 1;

  let start = 0;
  for (let i = 0; i < index; i += 1) start += lines[i].length + 1;
  const end = start + lines[index].length;

  element.focus();
  element.setSelectionRange(start, end);

  // Scroll the selection roughly into the middle. A textarea does not scroll to
  // a selection on its own when it is already focused, so a jump within the
  // same document would otherwise move the caret somewhere the reader cannot
  // see.
  const lineHeight = element.scrollHeight / Math.max(lines.length, 1);
  element.scrollTop = Math.max(0, lineHeight * index - element.clientHeight / 2);
}
