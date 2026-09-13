"use client";

import { useState } from "react";
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { retryOn5xx } from "@/lib/api";

/**
 * The query client.
 *
 * `refetchOnWindowFocus` is true for the same reason ApexTick sets it: the
 * thing on screen moves while nobody is looking at it. Here it moves rather
 * more — an operator alt-tabs to a terminal, runs something, and comes back
 * expecting the console to have noticed.
 *
 * `retry: retryOn5xx` rather than the library default of 1. The default retries
 * everything once, which doubles up 401, 403 and 404 — three answers that will
 * not change on a second ask. On this API a 404 is a routine answer (a job id
 * typed into the URL bar) and asking twice for every one of them is pure waste.
 *
 * Note what is NOT set here. There is no global `refetchInterval`: polling
 * cadence is a per-query decision because the two hot endpoints have very
 * different costs. GET /jobs/{id} is cheap under an ETag — a 304 costs almost
 * nothing and the ETag is the job Version, which exists specifically for this.
 * GET /ready is NOT cheap: it calls QueueDepth, which on the in-memory store
 * walks every job and every step under the same global mutex that serialises
 * Claim, Finish and Heartbeat, and on PostgreSQL is a count(*) over a predicate
 * with two correlated NOT EXISTS subqueries. A single global interval fast
 * enough for the first would put the second on the job-detail hot path and make
 * the console a load generator against the runtime it is meant to observe.
 */
export default function Providers({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            refetchOnWindowFocus: true,
            retry: retryOn5xx,
          },
        },
      }),
  );

  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}
