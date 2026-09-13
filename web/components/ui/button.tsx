"use client";

import Link from "next/link";
import type { ReactNode } from "react";
import { cn } from "@/lib/cn";
import { ArrowUpRight } from "./icons";

type Variant = "primary" | "outline" | "ghost" | "danger";
type Size = "sm" | "md" | "lg";

interface CommonProps {
  children: ReactNode;
  variant?: Variant;
  size?: Size;
  className?: string;
  arrow?: boolean;
  disabled?: boolean;
}

/**
 * A discriminated union rather than an `as` prop, so the wrong combination is a
 * type error at the call site instead of a runtime surprise: a link cannot take
 * an onClick, and a button cannot take an href.
 */
type ButtonAsLink = CommonProps & { href: string; onClick?: never; type?: never };
type ButtonAsButton = CommonProps & {
  href?: never;
  onClick?: () => void;
  type?: "button" | "submit";
};

type ButtonProps = ButtonAsLink | ButtonAsButton;

const sizes: Record<Size, string> = {
  sm: "h-9 px-4 text-[0.8rem]",
  md: "h-11 px-5 text-[0.9rem]",
  lg: "h-14 px-7 text-[0.95rem]",
};

/**
 * Variants are a plain lookup table composed through cn(), which is how this
 * house expresses variance. Adding one means adding a key here — not reaching
 * for cva or tailwind-variants, neither of which is installed and both of
 * which would change how every component in the system is read.
 *
 * `danger` is the one addition over ApexTick's three, and it is the hardcoded
 * #ff6b6b rather than a state token. Two reasons. The accent never means
 * destroy — arc blue is interactive chrome and nothing else — and the state
 * palette's FAILED red means "this step failed", which is an outcome the
 * runtime reported, not an action the reader is about to take. Cancelling a
 * job is not the same thing as a job having failed, and the button must not
 * borrow the colour that says it did.
 */
const variants: Record<Variant, string> = {
  primary: "bg-accent text-accent-ink hover:brightness-105 border border-transparent",
  outline:
    "bg-transparent text-bone border border-line-2 hover:border-bone hover:bg-bone/[0.04]",
  ghost: "bg-transparent text-bone border border-transparent hover:bg-bone/[0.06]",
  danger:
    "bg-transparent text-[#ff6b6b] border border-[#ff6b6b]/40 hover:border-[#ff6b6b] hover:bg-[#ff6b6b]/[0.08]",
};

export function Button(props: ButtonProps) {
  const {
    children,
    variant = "primary",
    size = "md",
    className,
    arrow = false,
    disabled = false,
  } = props;

  const classes = cn(
    "group/btn relative inline-flex items-center justify-center gap-2 rounded-full font-medium tracking-tight",
    "transition-[background-color,border-color,filter,transform] duration-300 ease-out",
    // The global :focus-visible rule draws a 2px accent outline at a 3px
    // offset, which a rounded pill on a bordered surface can clip. Restating
    // it here keeps the ring outside the border box.
    "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
    disabled && "pointer-events-none opacity-40",
    sizes[size],
    variants[variant],
    className,
  );

  const inner = (
    <>
      <span>{children}</span>
      {arrow && (
        <ArrowUpRight className="h-4 w-4 transition-transform duration-300 ease-out group-hover/btn:translate-x-0.5 group-hover/btn:-translate-y-0.5" />
      )}
    </>
  );

  // ApexTick's `magnetic` prop is deliberately not carried over. A control that
  // slides toward the cursor is charm on a marketing page and a liability on a
  // console where the neighbouring control cancels a running job.
  if ("href" in props && props.href !== undefined) {
    return (
      <Link href={props.href} className={classes} aria-disabled={disabled || undefined}>
        {inner}
      </Link>
    );
  }

  return (
    <button
      type={("type" in props && props.type) || "button"}
      onClick={"onClick" in props ? props.onClick : undefined}
      disabled={disabled}
      className={classes}
    >
      {inner}
    </button>
  );
}
