"use client";

import { useQuery } from "@tanstack/react-query";
import { ApiError, RM_BASE } from "@/lib/api";
import type { ReadyResponse } from "@/lib/types";

/**
 * GET /api/v1/ready, on a deliberately slow interval.
 *
 * FIVE SECONDS, AND NOT LESS. This endpoint is not free: it calls QueueDepth,
 * which on the in-memory store walks every job and every step under the same
 * global mutex that serialises Claim, Finish and Heartbeat, and on PostgreSQL
 * is a count(*) over a predicate with two correlated NOT EXISTS subqueries. A
 * console polling it at the rate the job screen polls a job would be a load
 * generator pointed at the runtime it exists to observe — and it would contend
 * for the very lock the workers need, so the number it reported would be partly
 * its own fault.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS ONE HOOK DOES NOT USE apiFetch.
 *
 * `ready` writes its body with writeJSON and not with writeError, so a 503
 * carries a populated readyResponse — `status: "draining"`, `reason: "shutting
 * down"` — and NOT the `{error:{...}}` envelope. apiFetch is right to throw on
 * a non-2xx and right to look for the envelope, but here that would turn the
 * single most important sentence this endpoint ever says into a generic "503
 * Service Unavailable" error card, at the exact moment an operator needs the
 * specific answer.
 *
 * So the 503 is read as DATA. Every other non-2xx still becomes an ApiError, so
 * a 401 from a missing key behaves the way it does everywhere else on this
 * console.
 * ---------------------------------------------------------------------------
 */
export function useReady() {
  return useQuery({
    queryKey: ["ready"],
    queryFn: async (): Promise<ReadyResponse> => {
      const response = await fetch(`${RM_BASE}/ready`, { cache: "no-store" });

      if (response.ok || response.status === 503) {
        return (await response.json()) as ReadyResponse;
      }

      // Not the drain case: fall back to the envelope the rest of the API uses.
      let message = `${response.status} ${response.statusText}`.trim();
      try {
        const body = (await response.json()) as { error?: { message?: string } };
        if (body.error?.message) message = body.error.message;
      } catch {
        // A proxy failure or an empty body. The status is still the answer.
      }
      throw new ApiError(response.status, message);
    },
    refetchInterval: 5000,
    // A readiness number a couple of seconds old is fine. One from the last
    // time this tab happened to be focused is not, and the gap between those
    // two is where a reader decides the fleet is idle.
    staleTime: 2000,
  });
}

/** The three answers /ready gives, as one word for the chrome to switch on. */
export function readyTone(ready: ReadyResponse | undefined): "ok" | "draining" | "unavailable" {
  if (!ready) return "unavailable";
  if (ready.status === "ready") return "ok";
  if (ready.status === "draining") return "draining";
  return "unavailable";
}
