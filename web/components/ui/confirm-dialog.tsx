"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { cn } from "@/lib/cn";
import { Button } from "./button";

/**
 * A modal confirmation, on the native `<dialog>` element.
 *
 * `showModal()` is used rather than a div with a high z-index, and that choice
 * buys three things no hand-rolled overlay gets for free: the focus trap, the
 * inert background, and Escape. All three are requirements rather than polish
 * on the one dialog this console has, which stops a running job.
 *
 * The `m-auto` is not a style preference. Tailwind's preflight sets `margin: 0`
 * on every element, which overrides the user-agent stylesheet's `margin: auto`
 * that centres a `<dialog>` — without it the dialog pins itself to the top-left
 * corner of the viewport, which looks like a rendering bug and is one.
 *
 * `tone="danger"` uses #ff6b6b and never the accent, and never FAILED red. The
 * accent is interactive chrome and means nothing on its own; FAILED red means
 * "the runtime reported a failed step", and a job an operator deliberately
 * stops has not failed.
 */
export function ConfirmDialog({
  open,
  title,
  body,
  confirmLabel,
  onConfirm,
  onCancel,
  tone = "default",
  busy = false,
}: {
  open: boolean;
  title: string;
  body: ReactNode;
  confirmLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
  tone?: "default" | "danger";
  /** The confirm is in flight. The dialog stays open and says so, because
   *  closing it and leaving the outcome to a poll hides the failure case. */
  busy?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    if (open && !dialog.open) dialog.showModal();
    if (!open && dialog.open) dialog.close();
  }, [open]);

  return (
    <dialog
      ref={ref}
      // Escape closes a modal dialog natively, and it does so without telling
      // React — leaving `open` true and the dialog shut, after which nothing
      // can reopen it. Listening for `cancel` is what keeps the two in step.
      onCancel={(event) => {
        event.preventDefault();
        if (!busy) onCancel();
      }}
      className={cn(
        "m-auto w-[min(30rem,calc(100vw-2rem))] rounded-2xl border border-line bg-ink-2 p-7 text-bone",
        "backdrop:bg-ink/80",
      )}
    >
      <h2 className="display text-[1.1rem]">{title}</h2>
      <div className="mt-3 text-[0.82rem] leading-relaxed text-muted">{body}</div>

      <div className="mt-7 flex flex-wrap justify-end gap-3">
        <Button variant="ghost" size="sm" onClick={onCancel} disabled={busy}>
          Keep running
        </Button>
        <Button
          variant={tone === "danger" ? "danger" : "primary"}
          size="sm"
          onClick={onConfirm}
          disabled={busy}
        >
          {busy ? "Working…" : confirmLabel}
        </Button>
      </div>
    </dialog>
  );
}
