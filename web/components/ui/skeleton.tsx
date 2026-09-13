import { cn } from "@/lib/cn";

/**
 * A placeholder block at `bg-bone/[0.03]`.
 *
 * ApexTick's loading placeholder is a faint ellipsis and it has no skeletons at
 * all, so this needs a reason. The reason is reflow, and it is specific to one
 * screen: the job detail page gets the job snapshot before it gets the event
 * stream, which means it already knows exactly how many steps there are and
 * therefore exactly how many lanes the waterfall will have. Drawing those lanes
 * empty and then filling them is strictly better than an ellipsis that is
 * replaced by a chart of unknown height — the second one moves the whole page
 * under the reader's cursor at the moment the data they were waiting for
 * arrives.
 *
 * So a skeleton is used HERE and only here: where the final geometry is already
 * known. Where it is not known — a tile whose value is a number, a table whose
 * row count is unknown — the house answer stays the faint ellipsis, because a
 * skeleton in that position is a guess at a shape and guesses reflow too.
 *
 * It deliberately does not shimmer. On this product motion signals a state
 * change and nothing else; a pulsing rectangle would be the only animation on
 * screen that means nothing, competing for attention with the RUNNING dot,
 * which means a great deal.
 *
 * `aria-hidden` with the region marked `aria-busy` by the caller: announcing
 * "loading" once beats a screen reader enumerating eleven grey rectangles.
 */
export function Skeleton({
  className,
  rounded = "md",
}: {
  className?: string;
  rounded?: "none" | "sm" | "md" | "full";
}) {
  const radii = {
    none: "",
    sm: "rounded-sm",
    md: "rounded-md",
    full: "rounded-full",
  } as const;

  return <div aria-hidden className={cn("bg-bone/[0.03]", radii[rounded], className)} />;
}

/** A stack of text-height bars, for a paragraph or a record whose length is known. */
export function SkeletonLines({
  lines = 3,
  className,
}: {
  lines?: number;
  className?: string;
}) {
  return (
    <div aria-hidden className={cn("space-y-2", className)}>
      {/*
        The last line is short. A block of equal-length bars reads as a table;
        ragged-right reads as prose, which is what this stands in for.
      */}
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton
          key={i}
          rounded="sm"
          className={cn("h-3", i === lines - 1 ? "w-2/5" : "w-full")}
        />
      ))}
    </div>
  );
}
