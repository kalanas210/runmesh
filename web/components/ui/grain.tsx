/**
 * Fixed film-grain overlay. Sits above the page, never intercepts pointers.
 * The noise is a self-contained SVG turbulence filter, so there is no asset to load.
 *
 * Carried over from ApexTick at 0.04 rather than its 0.05. The grain exists to
 * stop a flat near-black from banding, and a hero photograph can absorb more of
 * it than a screen made of 1px hairlines and 3px bars can: at 0.05 the noise
 * starts competing with the chart's own texture, which is load-bearing here.
 */
export function Grain() {
  return (
    <div
      aria-hidden
      className="pointer-events-none fixed inset-0 z-[60] opacity-[0.04] mix-blend-soft-light"
      style={{
        backgroundImage:
          "url(\"data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='160' height='160'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.85' numOctaves='3' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)'/%3E%3C/svg%3E\")",
        backgroundSize: "160px 160px",
      }}
    />
  );
}
