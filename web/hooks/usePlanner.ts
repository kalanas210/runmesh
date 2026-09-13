"use client";

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiJson } from "@/lib/api";
import type { JobResponse, Plan, PlanResponse } from "@/lib/types";

/**
 * The two halves of the human-in-the-loop flow, as two mutations.
 *
 * ---------------------------------------------------------------------------
 * WHY PLANNING AND SUBMITTING ARE SEPARATE CALLS HERE, WHEN /goals DOES BOTH.
 *
 * `POST /api/v1/goals` is the autonomous path: goal in, running job out, no
 * human between them. It exists and it is the right endpoint for an agent.
 * This screen deliberately does not use it, because the entire argument for
 * `POST /api/v1/plans` — plans.go states it outright — is that a plan written
 * by a language model and about to run against a real cluster should be
 * VISIBLE before anything runs. Wiring the review surface to /goals would be
 * building the review screen and then skipping the review.
 *
 * So: /plans produces a plan and a trace and executes nothing, the reviewer
 * reads both and may edit the plan, and /jobs submits what they approved. The
 * submission goes through exactly the same admission gate as a curl request —
 * `submitPlan` is shared by createJob and createGoal — so nothing is skipped
 * by taking the long way round.
 * ---------------------------------------------------------------------------
 */

export interface PlanRequest {
  goal: string;
  /** Optional structured context the plan may refer to. Sent only when parsed. */
  context?: unknown;
  maxSteps?: number;
  name?: string;
}

/**
 * POST /api/v1/plans.
 *
 * No retry, and that is a deliberate override of the client default. A failed
 * plan generation has already spent model tokens; retrying it automatically
 * spends them again for a caller who did not ask, and the failures worth
 * retrying here (a 429, a transient model error) are ones a person should see
 * the Retry-After for rather than have silently absorbed.
 */
export function useCreatePlan() {
  return useMutation({
    mutationFn: (request: PlanRequest) =>
      apiJson<PlanResponse>("/plans", {
        method: "POST",
        json: {
          goal: request.goal,
          context: request.context,
          max_steps: request.maxSteps,
          name: request.name,
        },
      }),
    retry: false,
  });
}

/**
 * POST /api/v1/jobs — the approved plan, submitted.
 *
 * The idempotency key is minted per submission rather than per plan. Two
 * different keys for two presses of the button is the honest behaviour: the
 * key exists so that ONE logical submission survives a network timeout and a
 * client retry, not so that a reviewer who deliberately submits the same plan
 * twice gets one job. The API replays on a duplicate key and answers 200 with
 * `Idempotency-Replayed`, which would be a confusing answer to a person who
 * pressed submit on purpose.
 */
export function useSubmitPlan() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (plan: Plan) =>
      apiJson<JobResponse>("/jobs", {
        method: "POST",
        json: plan,
        headers: { "Idempotency-Key": newIdempotencyKey() },
      }),
    retry: false,
    onSuccess: (job) => {
      // Seed the detail screen's cache so the redirect that follows renders the
      // job immediately rather than showing a loading state for a row already
      // in hand.
      queryClient.setQueryData(["job", job.id], job);
      void queryClient.invalidateQueries({ queryKey: ["jobs"] });
    },
  });
}

/**
 * A key the API will accept: it is bounded at 128 bytes and is otherwise
 * opaque.
 *
 * `crypto.randomUUID` is not available in every context this could run in — it
 * requires a secure context, and a console served over plain HTTP on a
 * developer's machine is not one — so the fallback is not theoretical.
 */
function newIdempotencyKey(): string {
  const uuid = globalThis.crypto?.randomUUID?.();
  if (uuid) return `console-${uuid}`;
  return `console-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
}
