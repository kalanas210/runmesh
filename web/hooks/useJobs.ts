"use client";

import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { apiJson } from "@/lib/api";
import { jobsQueryKey, type JobsQuery } from "@/lib/jobs-query";
import type { JobListResponse } from "@/lib/types";

/**
 * GET /api/v1/jobs, cursor-paginated.
 *
 * `placeholderData: keepPreviousData` is the whole reason this is not three
 * lines. Without it, changing a filter or turning a page unmounts the table,
 * renders the loading state, and remounts it — so the most common interaction
 * on the screen produces a full-height flash of skeleton for a request that
 * usually completes in single-digit milliseconds against an in-memory store.
 * With it, the previous page stays on screen, marked stale by `isPlaceholder
 * Data`, and the rows swap when the answer lands. The table dims rather than
 * disappearing, which is also the honest rendering: those rows ARE still the
 * last answer the runtime gave.
 *
 * The poll is five seconds and not one. A list is a browsing surface — nobody
 * watches a job list for a state transition, they open the job — and listJobs
 * has no ETag, so every poll transfers every row of the page in full. One
 * second here would be fifty job objects a second for a screen nobody is
 * reading closely.
 */
export function useJobs(query: JobsQuery) {
  return useQuery({
    queryKey: jobsQueryKey(query),
    queryFn: () =>
      apiJson<JobListResponse>("/jobs", {
        query: {
          // An array becomes repeated `state=` keys, which is what listJobs
          // reads. A comma-joined value is one unknown state and a 400.
          state: query.states as string[],
          cursor: query.cursor,
          limit: query.limit,
        },
      }),
    refetchInterval: 5000,
    placeholderData: keepPreviousData,
  });
}
