import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * The two-column record: a label in the house kicker style, a value in tabular
 * numerals.
 *
 * A real `<dl>` rather than a grid of divs, because that is what it is, and
 * because a screen reader then announces "term, definition" pairs instead of
 * reading two unrelated columns in visual order. On the job header and the step
 * inspector this is most of the page's text, so getting the semantics right
 * here is worth more than it looks.
 *
 * Values are `ReactNode` and not `string` deliberately: half the rows on this
 * product are a pill, a duration component or a copy button, and a component
 * that took strings would have every caller working around it.
 */

export interface KeyValueRow {
  label: string;
  value: ReactNode;
  /** Dropped entirely rather than rendered as an em dash. For rows that are
   *  meaningless rather than merely absent — a cancel reason on a job nobody
   *  cancelled is not "unknown", it does not apply. */
  when?: boolean;
}

export function KeyValue({
  rows,
  columns = 1,
  className,
}: {
  rows: readonly KeyValueRow[];
  /** Two columns on a wide panel; one inside a narrow inspector. */
  columns?: 1 | 2;
  className?: string;
}) {
  const shown = rows.filter((row) => row.when !== false);
  if (shown.length === 0) return null;

  return (
    <dl
      className={cn(
        "grid gap-x-6 gap-y-3",
        columns === 2 ? "sm:grid-cols-2" : "grid-cols-1",
        className,
      )}
    >
      {shown.map((row) => (
        <div key={row.label} className="min-w-0">
          <dt className="kicker text-[0.5625rem]">{row.label}</dt>
          <dd className="tnum mt-1 break-words text-[0.82rem] text-bone-2">{row.value}</dd>
        </div>
      ))}
    </dl>
  );
}
