import type { ReactNode } from "react";
import { cn } from "@/lib/cn";
import { surface } from "./card";

/**
 * The empty card. Padding is `p-10`, matching the empty branch of ApexTick's
 * DataTable exactly, so an empty table and an empty page have the same weight.
 *
 * `title` and `hint` are two props rather than one block of copy because they
 * answer two different questions and the second one is the one that gets
 * dropped. The title says what is not here; the hint says what to do about it.
 * An empty state with no hint is a dead end, and on a console the dead end is
 * usually "no jobs yet" in front of someone who has never submitted one.
 *
 * The other half of getting this right is not this component's job but is worth
 * stating where it will be read: "nothing exists" and "nothing matches these
 * filters" are DIFFERENT empty states. Conflating them is the most common bug
 * of this kind — a reader with a stale state filter concludes the runtime is
 * idle. Whoever builds the job list must pass a different title and offer a
 * Clear filters action for the second.
 */
export function EmptyState({
  title,
  hint,
  action,
  glyph,
  className,
}: {
  title: string;
  hint?: ReactNode;
  action?: ReactNode;
  /** A 24x24 icon from the house set. Decorative; the title carries the meaning. */
  glyph?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn(surface, "p-10 text-center", className)}>
      {glyph && (
        <div aria-hidden className="mb-4 flex justify-center text-faint [&>svg]:h-6 [&>svg]:w-6">
          {glyph}
        </div>
      )}
      <p className="text-[0.9rem] text-muted">{title}</p>
      {hint && <p className="mt-2 text-[0.78rem] leading-relaxed text-faint">{hint}</p>}
      {action && <div className="mt-6 flex justify-center gap-3">{action}</div>}
    </div>
  );
}
