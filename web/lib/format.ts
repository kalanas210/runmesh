/**
 * Presentation helpers.
 *
 * Every Intl call below pins an explicit locale AND an explicit UTC time zone.
 * That is not pedantry: a Next app renders these strings on the server and then
 * again in the browser, and if the two disagree about the locale or the offset
 * React reports a hydration mismatch and throws the server's markup away. UTC
 * is also the right frame of reference on its own merits — an operator
 * comparing a step's start against a log line from a machine in another region
 * needs one clock, not two.
 */

const INSTANT = new Intl.DateTimeFormat("en-GB", {
  timeZone: "UTC",
  day: "2-digit",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

const TIME_ONLY = new Intl.DateTimeFormat("en-GB", {
  timeZone: "UTC",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
});

/** "12 Sep 2026, 03:24:11 UTC". The em dash is the house placeholder for absent. */
export function formatInstant(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return `${INSTANT.format(d)} UTC`;
}

/** "03:24:11" — for a time axis, where the date is already established. */
export function formatTimeOfDay(iso: string | null | undefined): string {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return TIME_ONLY.format(d);
}

/**
 * A duration, chosen so the magnitude is readable at a glance.
 *
 * The range this has to cover is enormous and that is the whole difficulty: a
 * step can take 3 milliseconds and a retry backoff can take forty minutes, and
 * both appear in the same column. So the unit changes with the magnitude and
 * the precision falls as the number grows — sub-second in milliseconds,
 * seconds to two decimals, then minutes and seconds, then hours.
 *
 * `precise` forces the exact millisecond count, which the hover card uses on a
 * bar whose width was clamped to the 3px minimum: the chart is lying about that
 * segment's size by necessity, so the number next to it must not.
 */
export function formatDuration(
  ms: number | null | undefined,
  precise = false,
): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return "—";
  if (precise) return `${Math.round(ms)}ms`;
  if (ms < 1000) return `${Math.round(ms)}ms`;

  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(2)}s`;

  const wholeSeconds = Math.round(seconds);
  const minutes = Math.floor(wholeSeconds / 60);
  const remainder = wholeSeconds % 60;
  if (minutes < 60) return `${minutes}m ${String(remainder).padStart(2, "0")}s`;

  const hours = Math.floor(minutes / 60);
  return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
}

/**
 * A short age for the freshness chip: "2s", "45s", "3m 20s", "1h 04m".
 *
 * Separate from formatDuration because it is read many times a second out of
 * the corner of an eye, and two decimal places of seconds ticking there would
 * be noise pretending to be information.
 */
export function formatAge(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "0s";
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${String(seconds % 60).padStart(2, "0")}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h ${String(minutes % 60).padStart(2, "0")}m`;
}

/**
 * `timeout_seconds` is the one duration on this API expressed in seconds, and
 * as a float. Everything else is integer milliseconds. This conversion exists
 * as a named function so the multiplication happens in one place and a reader
 * of a call site can see which unit they are in.
 */
export function timeoutToMs(timeoutSeconds: number): number {
  return timeoutSeconds * 1000;
}

/**
 * An offset from the start of the job: "+00:12.4", "+1:04:09".
 *
 * This exists for exactly one surface — the narrow-viewport card list, where
 * an absolute UTC timestamp does not fit across a phone and would be truncated
 * into uselessness. Everywhere there is room, the axis stays absolute, because
 * a relative time is a number that cannot leave the screen: it matches nothing
 * in a log line, a pod event or an incident timeline.
 *
 * Tenths below the hour and whole seconds above it, because the two questions
 * are different. Inside a minute a reader is comparing steps against each
 * other and a tenth is the difference; past an hour they are locating a moment
 * and a tenth is noise.
 */
export function formatOffset(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "+00:00.0";

  const totalSeconds = Math.floor(ms / 1000);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  if (hours > 0) {
    return `+${hours}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`;
  }
  const tenths = Math.floor((ms % 1000) / 100);
  return `+${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}.${tenths}`;
}
