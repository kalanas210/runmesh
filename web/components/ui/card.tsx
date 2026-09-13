import type { ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * The card surface.
 *
 * ApexTick has no Card primitive: the surface is the literal
 * `rounded-2xl border border-line bg-ink-2`, repeated thirty-two times, and the
 * house risk note is explicit that inventing a Card with different padding or a
 * different radius is the fastest way to make a sibling product read as a
 * different product.
 *
 * This component exists anyway, and it resolves that tension by having no
 * opinions of its own: `surface` below is that literal, character for
 * character, and the padding scale is the one already in use over there — p-6
 * for a tile, p-7 for a dialog, p-10 for an empty state. It is a named
 * constant for a value that was already fixed, not a new abstraction.
 *
 * The reason to name it: this product has one screen whose cards are generated
 * in a loop from API data rather than hand-written, and a typo in a repeated
 * literal is invisible in review. `surface` is also exported so the places that
 * need the literal inline — a card that is really a <button>, or the narrow
 * viewport attempt list — can compose it without nesting an element they do
 * not want.
 */
export const surface = "rounded-2xl border border-line bg-ink-2";

type Pad = "none" | "sm" | "md" | "lg";

const pads: Record<Pad, string> = {
  none: "",
  sm: "p-4",
  md: "p-6",
  lg: "p-10",
};

export function Card({
  children,
  pad = "md",
  className,
}: {
  children: ReactNode;
  pad?: Pad;
  className?: string;
}) {
  return <div className={cn(surface, pads[pad], className)}>{children}</div>;
}
