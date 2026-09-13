"use client";

import { useCallback, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import {
  clearedFilters,
  isFiltered,
  jobsQueryToSearch,
  parseJobsQuery,
  withCursor,
  withStates,
  type JobsQuery,
} from "@/lib/jobs-query";
import type { JobState } from "@/lib/state";
import { useJobs } from "@/hooks/useJobs";
import { Button } from "@/components/ui/button";
import { Code } from "@/components/ui/code";
import { CursorPager } from "@/components/ui/cursor-pager";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorState } from "@/components/ui/error-state";
import { Freshness, stalenessOpacity } from "@/components/ui/freshness";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/shell/page-header";
import { useNow } from "@/lib/now";
import { JobStatBand } from "./job-stat-band";
import { JobTable } from "./job-table";
import { StateFilterBar } from "./state-filter-bar";

/**
 * The jobs index: find a job.
 *
 * ---------------------------------------------------------------------------
 * THE FILTER LIVES IN THE URL AND THE PAGE POSITION DOES NOT.
 *
 * The two things an operator does with a filtered job list are bookmark it and
 * paste it into an incident channel, and neither survives `useState`. So the
 * states are query parameters, parsed by a pure function with a test on it.
 *
 * The cursor is different, and the difference is deliberate. It IS in the URL
 * so a shared link lands on the page the sharer was looking at — but the stack
 * of cursors already visited is NOT, because there is no backward cursor on
 * this API and reconstructing one from a pasted URL is impossible. A reader who
 * opens a shared deep link can page older and cannot page newer, which is
 * honest; the alternative is a Newer button that silently does nothing.
 * ---------------------------------------------------------------------------
 *
 * All four required states are here and none of them is a spinner over the
 * whole page: LOADING draws the table's own skeleton at the height a page of
 * rows will take, ERROR is the request-id card, STALE dims through the
 * freshness chip, and EMPTY is TWO different states — see below, because
 * conflating them is the single most common bug of this kind.
 */
export function JobList() {
  const router = useRouter();
  const params = useSearchParams();
  const query = parseJobsQuery(new URLSearchParams(params.toString()));

  /**
   * The pages already visited, newest first. The API returns a forward cursor
   * only — `next_cursor`, id-descending — so "back" cannot be a request, it has
   * to be a remembered position. Component state rather than the URL, because
   * it describes this session's path through the list and not the list itself.
   */
  const [history, setHistory] = useState<string[]>([]);

  const jobs = useJobs(query);
  const now = useNow();

  const go = useCallback(
    (next: JobsQuery) => {
      // `replace` and not `push`: a filter toggle is a refinement of the
      // current view, and pushing each one makes the browser's Back button walk
      // backwards through every chip the reader tried instead of leaving the
      // page.
      router.replace(`/jobs${jobsQueryToSearch(next)}`, { scroll: false });
    },
    [router],
  );

  const toggle = useCallback(
    (state: JobState) => {
      // withStates drops the cursor, and that is a correctness fix rather than
      // tidiness: a cursor is a position in a FILTERED sequence, so keeping it
      // while changing the filter asks the API to continue a list that no
      // longer exists. The answer is a page from the middle of a different
      // result set — usually an empty one, which reads as "no jobs match" when
      // several do.
      setHistory([]);
      go(withStates(query, state));
    },
    [go, query],
  );

  const clear = useCallback(() => {
    setHistory([]);
    go(clearedFilters(query));
  }, [go, query]);

  const older = useCallback(() => {
    const cursor = jobs.data?.next_cursor;
    if (!cursor) return;
    setHistory((stack) => [...stack, query.cursor ?? ""]);
    go(withCursor(query, cursor));
  }, [go, jobs.data?.next_cursor, query]);

  const newer = useCallback(() => {
    setHistory((stack) => {
      const previous = stack[stack.length - 1];
      go(withCursor(query, previous === "" ? undefined : previous));
      return stack.slice(0, -1);
    });
  }, [go, query]);

  const rows = jobs.data?.jobs ?? [];
  const filtered = isFiltered(query);
  const age = now !== null && jobs.dataUpdatedAt ? now - jobs.dataUpdatedAt : 0;

  return (
    <>
      <PageHeader
        kicker="Jobs"
        title="Every job this runtime knows about"
        description="Newest first. The runtime pages by cursor and keeps no total, so the list is a window rather than a count."
        freshness={
          <Freshness updatedAt={jobs.dataUpdatedAt || null} error={jobs.isError ? jobs.error : undefined} />
        }
        action={
          <Button href="/submit" size="sm" arrow>
            Plan a job
          </Button>
        }
      />

      <div className="mt-8">
        <JobStatBand jobs={rows} loading={jobs.isPending} />
      </div>

      <div className="mt-8 flex flex-wrap items-center justify-between gap-4">
        <StateFilterBar value={query.states} onToggle={toggle} onClear={clear} />
      </div>

      <div
        className="mt-4 transition-opacity"
        style={{
          // Past a minute without an answer the content itself dims. A stale
          // table that looks exactly like a fresh one is the failure this whole
          // freshness apparatus exists to prevent.
          opacity: stalenessOpacity(age),
          transitionDuration: "var(--dur-state)",
        }}
      >
        {jobs.isPending ? (
          <LoadingRows />
        ) : jobs.isError ? (
          <ErrorState
            error={jobs.error}
            caption="the job list"
            onRetry={() => void jobs.refetch()}
          />
        ) : rows.length === 0 ? (
          filtered ? (
            /*
              The two empty states are DIFFERENT and must never share copy. A
              reader with a stale state filter in a bookmarked URL, shown "no
              jobs yet", concludes the runtime is idle when it is not — and
              goes looking for the fault somewhere else entirely.
            */
            <EmptyState
              title="No jobs match these filters"
              hint="The runtime may still be busy with jobs in other states."
              action={
                <Button variant="outline" size="sm" onClick={clear}>
                  Clear filters
                </Button>
              }
            />
          ) : (
            <EmptyState
              title="No jobs yet"
              hint="Nothing has been submitted to this runtime. Plan one in English, or submit a plan directly with curl."
              action={
                <div className="w-full max-w-xl text-left">
                  <Button href="/submit" size="sm" arrow>
                    Plan a job
                  </Button>
                  <Code
                    className="mt-5"
                    label="Submit a job with curl"
                    maxHeight={220}
                    value={CURL_EXAMPLE}
                  />
                </div>
              }
            />
          )
        ) : (
          <>
            <JobTable jobs={rows} stale={jobs.isPlaceholderData} />
            <CursorPager
              className="mt-4"
              count={rows.length}
              hasNewer={history.length > 0}
              hasOlder={!!jobs.data?.next_cursor}
              onNewer={newer}
              onOlder={older}
              busy={jobs.isFetching}
            />
          </>
        )}
      </div>
    </>
  );
}

/**
 * The loading state.
 *
 * A skeleton here rather than the house ellipsis, because this is the one place
 * the final geometry IS known: a page is up to fifty rows of a fixed height
 * inside a bordered surface, so drawing that shape and filling it cannot
 * reflow. Eight rows rather than fifty — enough to read as a table, not so many
 * that an empty runtime flashes a full screen of grey.
 */
function LoadingRows() {
  return (
    <div
      aria-busy
      aria-label="Loading jobs"
      className="rounded-2xl border border-line bg-ink-2 p-4"
    >
      {Array.from({ length: 8 }, (_, i) => (
        <div key={i} className="flex items-center gap-4 border-b border-line/60 py-3 last:border-b-0">
          <Skeleton className="h-3 w-40" rounded="sm" />
          <Skeleton className="h-3 w-56" rounded="sm" />
          <Skeleton className="ml-auto h-4 w-24" rounded="full" />
        </div>
      ))}
    </div>
  );
}

/** The real route, with the real headers. A curl a reader can paste is worth
 *  more than a sentence telling them a curl exists. */
const CURL_EXAMPLE = `curl -X POST http://localhost:8080/api/v1/jobs \\
  -H "Authorization: Bearer $RUNMESH_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "name": "hello runmesh",
    "steps": [{ "id": "greet", "tool": "echo", "params": { "message": "hi" } }]
  }'`;
