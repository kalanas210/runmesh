"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { cn } from "@/lib/cn";
import { Check, Copy } from "./icons";

/**
 * Copy to the clipboard, with the one piece of feedback that matters: whether
 * it worked.
 *
 * The confirmation is inline and local — the icon becomes a tick for two
 * seconds — rather than a toast, because this product has no floating surfaces
 * anywhere. Feedback is rendered next to the thing it describes, which is also
 * where the reader is already looking.
 *
 * `navigator.clipboard` is unavailable outside a secure context, and a console
 * served over plain HTTP on a developer's machine is exactly that case. So the
 * failure branch is real and it says "copy failed" rather than silently doing
 * nothing — a button that appears to work and does not is worse than one that
 * admits it cannot.
 */
export function CopyButton({
  value,
  label = "copy",
  className,
}: {
  value: string;
  label?: string;
  className?: string;
}) {
  const [state, setState] = useState<"idle" | "copied" | "failed">("idle");
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // A timeout that outlives its component calls setState on an unmounted tree.
  // On this screen that is not theoretical: the inspector unmounts the moment
  // a selection changes, which is often the click after this one.
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
      setState("copied");
    } catch {
      setState("failed");
    }
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(() => setState("idle"), 2000);
  }, [value]);

  return (
    <button
      type="button"
      onClick={() => void copy()}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border border-line-2 px-2.5 py-0.5",
        "font-mono text-[0.6rem] uppercase tracking-[0.14em] text-muted transition-colors",
        "hover:border-bone hover:text-bone",
        className,
      )}
      style={{ transitionDuration: "var(--dur-hover)" }}
    >
      {state === "copied" ? (
        <Check className="h-3 w-3" aria-hidden />
      ) : (
        <Copy className="h-3 w-3" aria-hidden />
      )}
      {/* role="status" so the change is announced, since the icon swap alone is
          invisible to a screen reader. */}
      <span role="status">
        {state === "copied" ? "copied" : state === "failed" ? "copy failed" : label}
      </span>
    </button>
  );
}
