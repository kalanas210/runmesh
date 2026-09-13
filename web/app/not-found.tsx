import Link from "next/link";

/**
 * Sized against the marketing 404's `clamp(3rem,13vw,9rem)` but smaller, and
 * that is the point: a console is somewhere people work, and a full-bleed
 * typographic joke about being lost is charm the third time and an obstacle
 * the tenth. The useful content is the way back.
 */
export default function NotFound() {
  return (
    <div className="flex min-h-[60vh] flex-col justify-center">
      <p className="kicker">404</p>
      <h1 className="display mt-3 text-[clamp(2rem,6vw,4rem)]">
        There is nothing at this address
      </h1>
      <p className="mt-4 max-w-md text-[0.9rem] leading-relaxed text-muted">
        A job id that has been evicted, a route that was never built, or a typo.
        The job list is the fastest way back to whatever you were looking for.
      </p>
      <div className="mt-8 flex flex-wrap gap-4 text-[0.86rem]">
        <Link href="/" className="ulink text-bone">
          Overview
        </Link>
        <Link href="/jobs" className="ulink text-muted">
          Jobs
        </Link>
      </div>
    </div>
  );
}
