import { JOB_STATES, type JobState } from "./state";

/**
 * The job list's query, as a value.
 *
 * The filter lives in the URL rather than in component state, because the two
 * things an operator does with a filtered job list are bookmark it and paste it
 * into an incident channel, and neither survives `useState`. That makes the URL
 * the source of truth and makes parsing it a pure function worth testing: the
 * failure mode of getting this wrong is not a crash but a filter that silently
 * does nothing, which is indistinguishable from a quiet runtime.
 *
 * ---------------------------------------------------------------------------
 * TWO DECISIONS THAT LOOK LIKE FUSSINESS AND ARE NOT.
 *
 * 1. States are normalised into the order of JOB_STATES, never the order they
 *    were clicked. The array becomes part of the React Query key, and
 *    ["RUNNING","FAILED"] and ["FAILED","RUNNING"] are different keys for the
 *    same question — so an operator toggling two filters in the other order
 *    gets a second cache entry, a second network round trip, and a visible
 *    flash of the loading state for data that was already in hand.
 *
 * 2. An unrecognised `?state=` value is DROPPED rather than passed through.
 *    listJobs (jobs.go:132) runs every value through ParseState and answers 400
 *    for one it does not know, which would turn a stale bookmark from someone
 *    else's older build into a red error card instead of a job list. Dropping
 *    it is not hiding an error: the filter bar renders from the parsed value,
 *    so a dropped state visibly is not selected.
 * ---------------------------------------------------------------------------
 *
 * Only the six JOB_STATES are offered. jobEdges never rolls a job up to
 * SCHEDULED or RETRYING, so a filter bar carrying all eight would have two
 * chips that can never match anything.
 */

export interface JobsQuery {
  /** Repeatable `?state=`, in JOB_STATES order. Empty means no filter. */
  states: JobState[];
  /** Opaque, from `next_cursor`. Absent on the first page. */
  cursor?: string;
  /** The API accepts [1, 200] and defaults to 50. */
  limit?: number;
}

export const JOBS_PAGE_SIZE = 50;

const JOB_STATE_SET = new Set<string>(JOB_STATES);

/** Sort into wire order so the query key is stable however the user clicked. */
function normaliseStates(raw: readonly string[]): JobState[] {
  const chosen = new Set<string>();
  for (const value of raw) {
    const upper = value.trim().toUpperCase();
    if (JOB_STATE_SET.has(upper)) chosen.add(upper);
  }
  return JOB_STATES.filter((state) => chosen.has(state));
}

export function parseJobsQuery(params: URLSearchParams): JobsQuery {
  const query: JobsQuery = { states: normaliseStates(params.getAll("state")) };

  const cursor = params.get("cursor");
  if (cursor) query.cursor = cursor;

  const limit = Number(params.get("limit"));
  // Silently ignoring a nonsense limit rather than forwarding it: the API's own
  // 400 says "limit must be an integer in [1, 200]", which is a fine message
  // for a caller writing curl and a baffling one for a reader who typed a URL.
  if (Number.isInteger(limit) && limit >= 1 && limit <= 200) query.limit = limit;

  return query;
}

/**
 * Back to a query string, with a leading "?" or empty.
 *
 * The cursor is intentionally NOT preserved by the filter controls — see
 * `withStates` — but it is serialised here, because the pager needs it and
 * because a link someone shares should land on the page they were looking at.
 */
export function jobsQueryToSearch(query: JobsQuery): string {
  const params = new URLSearchParams();
  for (const state of query.states) params.append("state", state);
  if (query.cursor) params.set("cursor", query.cursor);
  if (query.limit) params.set("limit", String(query.limit));
  const search = params.toString();
  return search ? `?${search}` : "";
}

/**
 * Toggle one state on or off, and drop the cursor.
 *
 * Dropping the cursor is the correctness bit. A cursor is a position in a
 * filtered, id-descending sequence; keeping it while changing the filter asks
 * the API to continue a list that no longer exists, and the answer is a page
 * from the middle of a different result set — an empty page, usually, which
 * reads as "no jobs match" when in fact several do. That is the single most
 * plausible wrong-looking-right bug on this screen.
 */
export function withStates(query: JobsQuery, state: JobState): JobsQuery {
  const states = query.states.includes(state)
    ? query.states.filter((value) => value !== state)
    : normaliseStates([...query.states, state]);
  return { states, limit: query.limit };
}

/** Clear the filters, keeping nothing — including the cursor, for the reason above. */
export function clearedFilters(query: JobsQuery): JobsQuery {
  return { states: [], limit: query.limit };
}

/** Advance to the next page. The API is cursor-paginated with no total count,
 *  so there is no page number to hold and no "last page" to jump to. */
export function withCursor(query: JobsQuery, cursor: string | undefined): JobsQuery {
  return { ...query, cursor };
}

/** True when a filter is applied, which is what separates "no jobs match these
 *  filters" from "no jobs exist" — two empty states that must never be
 *  conflated, because the first one in front of the second one's copy tells an
 *  operator the runtime is idle when it is not. */
export function isFiltered(query: JobsQuery): boolean {
  return query.states.length > 0;
}

/**
 * The React Query key. An array rather than a string so the devtools show the
 * parts, and built from the normalised value so two spellings of the same
 * question share one cache entry.
 */
export function jobsQueryKey(query: JobsQuery): readonly unknown[] {
  return ["jobs", { states: query.states, cursor: query.cursor ?? null, limit: query.limit ?? null }];
}
