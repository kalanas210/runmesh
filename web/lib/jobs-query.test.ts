import { describe, expect, it } from "vitest";
import {
  clearedFilters,
  isFiltered,
  jobsQueryKey,
  jobsQueryToSearch,
  parseJobsQuery,
  withCursor,
  withStates,
} from "./jobs-query";

const q = (search: string) => parseJobsQuery(new URLSearchParams(search));

describe("parseJobsQuery", () => {
  it("normalises state order so the query key is click-order independent", () => {
    // Two spellings of one question must be one cache entry, or toggling the
    // same two filters in the other order refetches and flashes the loader.
    expect(q("state=FAILED&state=RUNNING").states).toEqual(q("state=RUNNING&state=FAILED").states);
    expect(q("state=FAILED&state=RUNNING").states).toEqual(["RUNNING", "FAILED"]);
  });

  it("drops an unknown state rather than forwarding it into a 400", () => {
    // ParseState answers 400 for a value it does not know, so a stale bookmark
    // would otherwise render a red card instead of a job list.
    expect(q("state=RUNNING&state=BANANA").states).toEqual(["RUNNING"]);
  });

  it("rejects the two step-only states, which a job can never hold", () => {
    expect(q("state=SCHEDULED&state=RETRYING").states).toEqual([]);
  });

  it("accepts lowercase, because a URL typed by hand usually is", () => {
    expect(q("state=running").states).toEqual(["RUNNING"]);
  });

  it("deduplicates a repeated state", () => {
    expect(q("state=RUNNING&state=RUNNING").states).toEqual(["RUNNING"]);
  });

  it("keeps a limit inside the API's range and ignores one outside it", () => {
    expect(q("limit=25").limit).toBe(25);
    expect(q("limit=0").limit).toBeUndefined();
    expect(q("limit=500").limit).toBeUndefined();
    expect(q("limit=abc").limit).toBeUndefined();
  });

  it("carries the cursor through verbatim, since it is opaque", () => {
    expect(q("cursor=01JABC").cursor).toBe("01JABC");
  });
});

describe("jobsQueryToSearch", () => {
  it("round-trips", () => {
    const search = jobsQueryToSearch({ states: ["RUNNING", "FAILED"], cursor: "c1", limit: 10 });
    expect(parseJobsQuery(new URLSearchParams(search))).toEqual({
      states: ["RUNNING", "FAILED"],
      cursor: "c1",
      limit: 10,
    });
  });

  it("is empty for an unfiltered first page", () => {
    expect(jobsQueryToSearch({ states: [] })).toBe("");
  });

  it("repeats the key rather than joining with commas", () => {
    // listJobs reads q["state"], so a comma-joined value is one unknown state.
    expect(jobsQueryToSearch({ states: ["RUNNING", "FAILED"] })).toBe("?state=RUNNING&state=FAILED");
  });
});

describe("changing the filter", () => {
  it("drops the cursor, because it indexes a list that no longer exists", () => {
    // Continuing a cursor across a filter change returns a page from the middle
    // of a different result set — usually empty, which reads as "nothing
    // matches" when several jobs do.
    expect(withStates({ states: [], cursor: "c1" }, "RUNNING").cursor).toBeUndefined();
    expect(clearedFilters({ states: ["RUNNING"], cursor: "c1" }).cursor).toBeUndefined();
  });

  it("toggles off a state that was on", () => {
    expect(withStates({ states: ["RUNNING", "FAILED"] }, "RUNNING").states).toEqual(["FAILED"]);
  });

  it("keeps the page size across a filter change", () => {
    expect(withStates({ states: [], limit: 20 }, "RUNNING").limit).toBe(20);
  });
});

describe("isFiltered", () => {
  it("separates the two empty states", () => {
    expect(isFiltered({ states: [] })).toBe(false);
    expect(isFiltered({ states: ["CANCELLED"] })).toBe(true);
  });
});

describe("jobsQueryKey", () => {
  it("differs by cursor, so a page is not served from the previous page's cache", () => {
    expect(jobsQueryKey({ states: [] })).not.toEqual(jobsQueryKey(withCursor({ states: [] }, "c1")));
  });
});
