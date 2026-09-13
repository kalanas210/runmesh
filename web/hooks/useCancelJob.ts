"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiJson } from "@/lib/api";
import type { JobResponse } from "@/lib/types";

/**
 * POST /api/v1/jobs/{id}/cancel.
 *
 * ---------------------------------------------------------------------------
 * THE ANSWER IS 202 AND THE UI MUST NOT PRETEND IT WAS 200.
 *
 * Cancellation is a REQUEST. The handler flags the job and returns; steps a
 * worker already owns keep running until their next heartbeat delivers the
 * news, which can be seconds away and lands on a different replica. So the body
 * that comes back honestly says `"state": "RUNNING"` with `cancel_requested_at`
 * set, and that is a correct response rather than a race.
 *
 * The consequence for this hook is that it must write the returned job into the
 * cache and then GET OUT OF THE WAY. The tempting alternatives are both lies: a
 * button that flips the pill to CANCELLED immediately claims something that has
 * not happened, and an optimistic update that is then reverted by the next poll
 * shows the reader a state transition running backwards. The pill derives
 * CANCELLING from `cancel_requested_at` precisely so this moment can be drawn
 * truthfully, and the poll that follows will report the real settling.
 * ---------------------------------------------------------------------------
 *
 * A 409 is the other case worth naming: the job already reached a terminal
 * state between the screen rendering and the button being pressed. The dialog
 * surfaces the API's own sentence for it, which says so.
 */
export function useCancelJob(jobId: string) {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () =>
      apiJson<JobResponse>(`/jobs/${jobId}/cancel`, { method: "POST", json: {} }),
    onSuccess: (job) => {
      // The 202 body is a full job row, one version newer than whatever is
      // cached, so writing it is strictly better than invalidating: it updates
      // the screen in the same frame instead of after a round trip.
      queryClient.setQueryData(["job", jobId], job);
      // The list holds a copy of the same row and has no ETag of its own, so
      // it is marked stale rather than rewritten.
      void queryClient.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}
