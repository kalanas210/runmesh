import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * The inline banner. This product's only feedback surface, and deliberately
 * the only one.
 *
 * ApexTick has no floating layer anywhere: no toasts, no snackbars, no corner
 * notifications. A sibling product that grows them stops reading as a sibling,
 * and on an operator console the argument is stronger than house style. A toast
 * is a message that disappears on a timer, placed away from the thing it is
 * about, over content the reader may be in the middle of. The messages this
 * screen has to deliver — the runtime is draining, the stream degraded to
 * polling, this history is incomplete — are all conditions rather than events,
 * and a condition that vanishes after four seconds was never reported.
 *
 * So a Notice sits in the layout, next to what it describes, and stays until
 * the condition does.
 *
 * `warn` borrows the RETRYING amber and `error` the #ff6b6b that the danger
 * button uses. Neither borrows the FAILED red: that colour means "the runtime
 * reported a failed step" and reusing it for "your filter matched nothing"
 * would put a data colour on a chrome message, on the one screen where colour
 * is data.
 */

type Tone = "info" | "warn" | "error";

const tones: Record<Tone, string> = {
  info: "border-line-2 text-muted",
  warn: "border-st-retrying/40 bg-st-retrying/[0.05] text-st-retrying",
  error: "border-[#ff6b6b]/40 bg-[#ff6b6b]/[0.05] text-[#ff6b6b]",
};

export function Notice({
  tone = "info",
  title,
  children,
  action,
  className,
}: {
  tone?: Tone;
  title?: string;
  children?: ReactNode;
  action?: ReactNode;
  className?: string;
}) {
  return (
    <div
      // `alert` is assertive and interrupts a screen reader mid-sentence, which
      // is right for a failure and wrong for a note. `status` is polite and
      // waits its turn.
      role={tone === "error" ? "alert" : "status"}
      className={cn(
        "flex flex-wrap items-start gap-x-4 gap-y-2 rounded-xl border px-4 py-3 text-[0.78rem] leading-relaxed",
        tones[tone],
        className,
      )}
    >
      <div className="min-w-0 flex-1">
        {title && <p className="font-medium">{title}</p>}
        {children && <div className={cn(title && "mt-1", "text-[0.75rem] opacity-90")}>{children}</div>}
      </div>
      {action}
    </div>
  );
}
