"use client";

import { cn } from "@/lib/cn";
import { ChevronLeft, ChevronRight } from "./icons";

/**
 * Older / newer, and never a page number.
 *
 * ---------------------------------------------------------------------------
 * WHY THE HOUSE PAGINATION COMPONENT CANNOT BE USED HERE.
 *
 * ApexTick paginates with page numbers, which requires a total count. This API
 * has none: `GET /api/v1/jobs` is cursor-paginated in id-descending order and
 * returns `next_cursor` when there is more, nothing when there is not. There is
 * no `total`, no `pages`, and no endpoint that counts. A page-number control
 * driven by that data would have to invent its denominator, and a control whose
 * numbers are invented is worse than no control — a reader who sees "page 2 of
 * 7" believes there are seven.
 *
 * So the vocabulary is directional. "Older" because the sort is by id
 * descending and ids are time-ordered, which makes the direction meaningful to
 * the reader rather than an arbitrary next/previous.
 * ---------------------------------------------------------------------------
 *
 * Going back is a STACK and not a cursor. The API gives a forward cursor only,
 * so there is no token for "the page before this one"; the caller keeps the
 * cursors it has already used and pops one. That is why `onNewer` takes no
 * argument and why `canGoNewer` is the caller's knowledge rather than this
 * component's.
 */
export function CursorPager({
  count,
  hasNewer,
  hasOlder,
  onNewer,
  onOlder,
  busy = false,
  className,
}: {
  /** Rows on this page. Printed because it is the only honest quantity here. */
  count: number;
  hasNewer: boolean;
  hasOlder: boolean;
  onNewer: () => void;
  onOlder: () => void;
  busy?: boolean;
  className?: string;
}) {
  const button =
    "inline-flex items-center gap-1.5 rounded-full border border-line-2 px-3 py-1 " +
    "font-mono text-[0.6rem] uppercase tracking-[0.14em] text-muted transition-colors " +
    "hover:border-bone hover:text-bone disabled:pointer-events-none disabled:opacity-35";

  return (
    <nav
      aria-label="Job list pages"
      className={cn("flex flex-wrap items-center justify-between gap-3", className)}
    >
      <p className="tnum text-[0.68rem] text-faint">
        {count} {count === 1 ? "job" : "jobs"} on this page
        {/* Said plainly, because the absence of a total is a property of the
            API and not an omission in this control. */}
        {hasOlder ? " · more exist" : ""}
      </p>

      <div className="flex items-center gap-2">
        <button
          type="button"
          className={button}
          onClick={onNewer}
          disabled={!hasNewer || busy}
          style={{ transitionDuration: "var(--dur-hover)" }}
        >
          <ChevronLeft className="h-3 w-3" aria-hidden />
          newer
        </button>
        <button
          type="button"
          className={button}
          onClick={onOlder}
          disabled={!hasOlder || busy}
          style={{ transitionDuration: "var(--dur-hover)" }}
        >
          older
          <ChevronRight className="h-3 w-3" aria-hidden />
        </button>
      </div>
    </nav>
  );
}
