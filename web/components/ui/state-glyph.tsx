import { cn } from "@/lib/cn";
import { STATE_GLYPH, type GlyphName, type StepState } from "@/lib/state";

/**
 * The second carrier of state, after the word and before the colour.
 *
 * These are drawn at 10x10 rather than reusing the 24x24 icon set because they
 * sit inside a pill whose cap height is about 8px, and a 24-unit grid scaled
 * that far down puts its 1.6 strokes on fractional device pixels — the ring
 * ends up a grey smudge. At this size the strokes are heavier in proportion
 * (1.6 on a 12-unit box) so each shape survives.
 *
 * They are hand-drawn SVG and never emoji. An emoji is a font-dependent colour
 * image: it ignores currentColor, renders differently on every platform, and
 * on Windows would land as a second typeface inside a mono pill. The entire
 * purpose of a glyph here is to be a carrier that does not depend on colour,
 * and a colour image is the one thing that cannot do that job.
 */

function Shape({ name }: { name: GlyphName }) {
  switch (name) {
    case "ring-hollow":
      return <circle cx="6" cy="6" r="3.6" />;
    case "ring-half":
      return (
        <>
          <circle cx="6" cy="6" r="3.6" />
          <path d="M6 2.4a3.6 3.6 0 0 1 0 7.2z" fill="currentColor" stroke="none" />
        </>
      );
    case "dot-pulse":
      return <circle cx="6" cy="6" r="3" fill="currentColor" stroke="none" />;
    case "retry":
      return <path d="M2.4 6a3.6 3.6 0 1 1 1.2 2.7M2.4 9.2V6.6h2.6" />;
    case "check":
      return <path d="m2.6 6.2 2.3 2.3 4.5-4.8" />;
    case "cross":
      return <path d="M3 3l6 6M9 3l-6 6" />;
    case "clock":
      return (
        <>
          <circle cx="6" cy="6" r="3.8" />
          <path d="M6 3.6V6l1.7 1.1" />
        </>
      );
    case "slash":
      return (
        <>
          <circle cx="6" cy="6" r="3.8" />
          <path d="m3.3 8.7 5.4-5.4" />
        </>
      );
  }
}

export function StateGlyph({
  state,
  className,
}: {
  state: StepState;
  className?: string;
}) {
  const name = STATE_GLYPH[state];

  // RUNNING is the one glyph with motion: a solid dot inside an expanding halo.
  // The halo is a sibling element rather than an SVG animation so the global
  // reduced-motion block can reach it by attribute, and so it can be scaled
  // past the SVG's own viewBox without being clipped.
  if (name === "dot-pulse") {
    return (
      <span
        aria-hidden
        className={cn("relative inline-flex h-[10px] w-[10px] items-center justify-center", className)}
      >
        <span
          data-pulse
          className="absolute inline-flex h-[6px] w-[6px] rounded-full bg-current"
          style={{ animation: "rm-pulse 2s var(--ease-out-expo) infinite" }}
        />
        <span className="relative inline-flex h-[6px] w-[6px] rounded-full bg-current" />
      </span>
    );
  }

  return (
    <svg
      viewBox="0 0 12 12"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
      className={cn("h-[10px] w-[10px] shrink-0", className)}
    >
      <Shape name={name} />
    </svg>
  );
}
