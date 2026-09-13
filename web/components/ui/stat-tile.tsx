import type { ReactNode } from "react";
import { cn } from "@/lib/cn";
import { surface } from "./card";

/**
 * One number, its label, and the sentence that says what population it is
 * about.
 *
 * `hint` is not decoration and it is not optional in practice. Every number on
 * this product's stat bands is about a DIFFERENT population: the queue depth is
 * the whole runtime, the failure rate is the fifty rows currently on screen,
 * the inflight count is this process. A tile that prints "4" over the word
 * FAILED with nothing else invites the reader to believe it is the fleet, and
 * on a cursor-paginated API with no aggregate endpoint that belief is simply
 * wrong. The hint is where the scope goes.
 *
 * The loading state is ApexTick's faint ellipsis and not a skeleton. The final
 * geometry of a tile is known — it is a tile — but the WIDTH of the number is
 * not, and a grey bar that is replaced by a two-character value is a reflow
 * dressed up as a placeholder.
 */
export function StatTile({
  label,
  value,
  hint,
  loading = false,
  tone = "default",
  className,
}: {
  label: string;
  value: ReactNode;
  /** Which population this number describes. Almost always worth saying. */
  hint?: ReactNode;
  loading?: boolean;
  /** `warn` for a number that is bad news at a glance — a non-zero failure
   *  count, a draining runtime. Never used to mean "this is a state". */
  tone?: "default" | "warn" | "muted";
  className?: string;
}) {
  return (
    <div className={cn(surface, "p-5", className)}>
      <p className="kicker text-[0.5625rem]">{label}</p>
      <p
        className={cn(
          "tnum mt-2 text-[1.6rem] leading-none",
          tone === "warn" ? "text-st-retrying" : tone === "muted" ? "text-muted" : "text-bone",
        )}
        aria-busy={loading || undefined}
      >
        {loading ? <span className="text-faint">…</span> : value}
      </p>
      {hint && <p className="mt-2 text-[0.68rem] leading-snug text-faint">{hint}</p>}
    </div>
  );
}
