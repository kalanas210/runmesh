import type { ElementType, ReactNode } from "react";
import { cn } from "@/lib/cn";

/**
 * The eyebrow label: mono, 0.6875rem, 0.28em tracking, uppercase, --muted.
 *
 * The styling lives in the `.kicker` class in globals.css and this component
 * only chooses the element, because a kicker is a different KIND of thing
 * depending on where it sits. Above a page title it is a <span> and purely
 * decorative. Heading a section it is an <h2>, and it is the only heading that
 * section has — a screen reader's heading list is how a keyboard user
 * navigates a dense operator screen, and a page whose section titles are all
 * anonymous spans gives them nothing to navigate by.
 *
 * Hence `as`. The default is span, so the careless case is the harmless one.
 */
export function Kicker({
  children,
  as: Tag = "span" as ElementType,
  className,
}: {
  children: ReactNode;
  as?: ElementType;
  className?: string;
}) {
  return <Tag className={cn("kicker", className)}>{children}</Tag>;
}
