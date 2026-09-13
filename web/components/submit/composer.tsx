"use client";

import { useCallback, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import {
  apiDetails,
  apiErrorMessage,
  apiRetryAfter,
  apiStatus,
  apiTrace,
} from "@/lib/api";
import { formatDuration } from "@/lib/format";
import { useNow } from "@/lib/now";
import { STARTER_PLAN, formatPlan, parsePlanDraft } from "@/lib/planner-view";
import type { PlannerTrace } from "@/lib/types";
import { useCreatePlan, useSubmitPlan } from "@/hooks/usePlanner";
import { Button } from "@/components/ui/button";
import { Notice } from "@/components/ui/notice";
import { PageHeader } from "@/components/shell/page-header";
import { PlanEditor, focusLine } from "./plan-editor";
import { ProblemList } from "./problem-list";
import { TracePanel } from "./trace-panel";

/**
 * Propose, review, submit — the human-in-the-loop surface the Go side built
 * POST /api/v1/plans for.
 *
 * ---------------------------------------------------------------------------
 * WHY THIS SCREEN DOES NOT USE /goals.
 *
 * `POST /api/v1/goals` takes a sentence and returns a RUNNING JOB. It is the
 * right endpoint for an agent and it is one call instead of two. This screen
 * deliberately does not use it, because the entire argument for /plans —
 * plans.go states it outright — is that a plan written by a language model and
 * about to run against a real cluster should be visible BEFORE anything runs.
 * Wiring the review surface to the autonomous endpoint would be building the
 * review screen and then skipping the review.
 *
 * So the flow is: /plans generates and validates and executes nothing; the
 * reviewer reads the plan and the whole trace beside it, and may edit the plan;
 * /jobs submits what they approved. Nothing is skipped by the long way round —
 * `submitPlan` is shared by createJob and createGoal, so an approved plan meets
 * the same admission control, the same per-tool validation, the same execution
 * policy and the same idempotency replay as a curl request.
 * ---------------------------------------------------------------------------
 *
 * THE FOUR FAILURE MODES ARE FOUR DIFFERENT SCREENS, not one error box:
 *
 *   501  no planner is configured. Names the variable that turns one on, since
 *        that is the entire fix and it is not discoverable from the message.
 *   422  the planner could not produce a valid plan. The trace is attached to
 *        this response ON PURPOSE and is rendered in full — every attempt, and
 *        what was wrong with each — because without it a refused goal is a
 *        refusal about a plan the reader never saw.
 *   429  rate limited. Surfaces Retry-After, because "try again" without a
 *        number is advice nobody can follow.
 *   400  the plan the reviewer submitted is invalid. Per-field, anchored to the
 *        editor's lines, never a toast.
 */
export function Composer() {
  const router = useRouter();
  const now = useNow();

  const [goal, setGoal] = useState("");
  const [name, setName] = useState("");
  const [draft, setDraft] = useState(STARTER_PLAN);
  const [trace, setTrace] = useState<PlannerTrace | null>(null);
  const [startedAt, setStartedAt] = useState<number | null>(null);

  const editor = useRef<HTMLTextAreaElement>(null);
  const plan = useCreatePlan();
  const submit = useSubmitPlan();

  const parsed = parsePlanDraft(draft);

  const onPlan = useCallback(() => {
    if (goal.trim() === "") return;
    setStartedAt(Date.now());
    plan.mutate(
      { goal: goal.trim(), name: name.trim() || undefined },
      {
        onSuccess: (response) => {
          setTrace(response.trace);
          // The accepted plan lands in the editor rather than in a read-only
          // panel, because the reviewer's one real power is to change it before
          // it runs.
          if (response.plan) setDraft(formatPlan(response.plan));
        },
        onError: (error) => {
          // The planner's 422 carries a trace beside the standard envelope and
          // nothing else on this API does. It is the most valuable half of that
          // response, so it is pulled out rather than lost with the error.
          setTrace(apiTrace(error) ?? null);
        },
      },
    );
  }, [goal, name, plan]);

  const onSubmit = useCallback(() => {
    if (!parsed.ok) return;
    submit.mutate(parsed.plan, {
      onSuccess: (job) => router.push(`/jobs/${job.id}`),
    });
  }, [parsed, router, submit]);

  const planStatus = apiStatus(plan.error);
  const submitDetails = apiDetails(submit.error);

  return (
    <>
      <PageHeader
        kicker="Submit"
        title="Describe the work, then read what the model proposed"
        description="The plan is generated and validated but not executed. Nothing runs until you submit it."
      />

      <div className="mt-8 grid gap-8 xl:grid-cols-[minmax(0,1fr)_minmax(0,28rem)]">
        {/* ------------------------------------------------------ the goal */}
        <div className="min-w-0 space-y-8">
          <section className="rounded-2xl border border-line bg-ink-2 p-6">
            <h2 className="kicker">The goal, in English</h2>

            <label htmlFor="goal" className="sr-only">
              What should this job do?
            </label>
            <textarea
              id="goal"
              value={goal}
              onChange={(event) => setGoal(event.target.value)}
              rows={4}
              placeholder="Fetch the three most recent release notes and summarise what changed."
              className="mt-3 block w-full resize-y rounded-xl border border-line bg-ink px-3.5 py-3 text-[0.85rem] leading-relaxed text-bone-2 placeholder:text-faint focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
            />

            <div className="mt-3 flex flex-wrap items-end gap-3">
              <div className="min-w-0 flex-1">
                <label htmlFor="plan-name" className="kicker text-[0.5625rem]">
                  name (optional)
                </label>
                <input
                  id="plan-name"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="overrides the generated name"
                  className="mt-1.5 block w-full rounded-xl border border-line bg-ink px-3.5 py-2 text-[0.8rem] text-bone-2 placeholder:text-faint focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent"
                />
              </div>

              <Button size="sm" onClick={onPlan} disabled={plan.isPending || goal.trim() === ""}>
                {plan.isPending ? "Planning…" : "Plan this goal"}
              </Button>
            </div>

            {plan.isPending && startedAt !== null && (
              /*
                An elapsed counter and not a progress bar. The planner is a
                single round trip with a retry loop inside it and reports
                nothing until it finishes — there is no attempt stream to
                follow — so a bar would be animating a number nobody measured.
                An elapsed count is true, and on a slow model it is the only
                thing that distinguishes "thinking" from "hung".
              */
              <p className="tnum mt-3 text-[0.7rem] text-muted" role="status">
                PLANNING… {formatDuration(now === null ? 0 : now - startedAt)}
              </p>
            )}

            <p className="mt-4 text-[0.68rem] leading-snug text-faint">
              The goal is untrusted text and reaches a model. The planner
              validates whatever comes back against this deployment&rsquo;s tool
              catalogue and limits before it is ever shown here.
            </p>
          </section>

          {plan.isError && <PlannerFailure error={plan.error} status={planStatus} />}

          {/* ---------------------------------------------------- the plan */}
          <section className="rounded-2xl border border-line bg-ink-2 p-6">
            <div className="flex flex-wrap items-baseline justify-between gap-3">
              <h2 className="kicker">The plan</h2>
              <p className="text-[0.65rem] text-faint">
                {trace
                  ? "Generated, validated, and not yet executed. Edit it before submitting."
                  : "A starter plan. Replace it, or plan a goal above."}
              </p>
            </div>

            <div className="mt-4">
              <PlanEditor
                value={draft}
                onChange={setDraft}
                invalid={!parsed.ok}
                editorRef={editor}
              />
            </div>

            {!parsed.ok && (
              <Notice tone="error" className="mt-3">
                {parsed.error}
              </Notice>
            )}

            {submitDetails.length > 0 && (
              // A 400 from POST /jobs is per-field and every problem arrives at
              // once. Anchored to the editor's lines rather than listed as
              // prose, because the field path is literally steps[2].tool.
              <ProblemList
                details={submitDetails}
                source={draft}
                onJump={(line) => focusLine(editor.current, line)}
              />
            )}

            {submit.isError && submitDetails.length === 0 && (
              <Notice tone="error" className="mt-3">
                {apiErrorMessage(submit.error, "The runtime refused this plan.")}
                {apiStatus(submit.error) === 429 && (
                  <>
                    {" "}
                    The queue is full. Retry after{" "}
                    {apiRetryAfter(submit.error) ?? 1} second
                    {(apiRetryAfter(submit.error) ?? 1) === 1 ? "" : "s"}.
                  </>
                )}
              </Notice>
            )}

            <div className="mt-5 flex flex-wrap items-center gap-3">
              <Button size="sm" onClick={onSubmit} disabled={!parsed.ok || submit.isPending}>
                {submit.isPending ? "Submitting…" : "Submit for execution"}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  setDraft(STARTER_PLAN);
                  submit.reset();
                }}
              >
                Reset to the starter plan
              </Button>
            </div>
          </section>
        </div>

        {/* ----------------------------------------------------- the trace */}
        <div className="min-w-0">
          {trace ? (
            <TracePanel trace={trace} className="xl:sticky xl:top-6" />
          ) : (
            <section className="rounded-2xl border border-line bg-ink-2 p-6">
              <h2 className="kicker">Planner trace</h2>
              <p className="mt-3 text-[0.78rem] leading-relaxed text-muted">
                Once a goal is planned, every round trip to the model appears
                here: which model, what it was offered, what it proposed, what
                was wrong with each rejected candidate, and what it cost.
              </p>
              <p className="mt-3 text-[0.68rem] leading-snug text-faint">
                The trace arrives on the refusal as well as on the success. A
                goal the planner could not satisfy is the case it exists for.
              </p>
            </section>
          )}
        </div>
      </div>
    </>
  );
}

/**
 * The planner's own failures, each answered with the thing that fixes it.
 *
 * A single "something went wrong" card would be defensible for three of these
 * and is wrong for the fourth: a 501 is not a failure at all, it is a
 * deployment that has no planner, and the fix is one environment variable that
 * the error message does not name.
 */
function PlannerFailure({ error, status }: { error: unknown; status?: number }) {
  if (status === 501) {
    return (
      <Notice tone="info" title="No planner is configured on this deployment">
        <p>
          Set <span className="tnum text-bone-2">RUNMESH_PLANNER=heuristic</span>{" "}
          for the offline planner, or{" "}
          <span className="tnum text-bone-2">RUNMESH_PLANNER=gemini</span> with a
          key, and restart the server. Until then a plan has to be written by
          hand — the editor below works either way, and submitting it needs no
          planner at all.
        </p>
      </Notice>
    );
  }

  if (status === 429) {
    const seconds = apiRetryAfter(error);
    return (
      <Notice tone="warn" title="The planner is rate limited">
        {apiErrorMessage(error, "The model refused another request just now.")}
        {seconds !== undefined && (
          <>
            {" "}
            Retry after <span className="tnum">{seconds}</span> second
            {seconds === 1 ? "" : "s"}.
          </>
        )}
      </Notice>
    );
  }

  if (status === 422) {
    return (
      <Notice tone="warn" title="The planner could not produce a valid plan">
        Every attempt it made is listed in the trace, with what the validator
        rejected about each. Narrowing the goal, or naming the tools it should
        use, usually resolves it — the catalogue is on the Tools screen.
      </Notice>
    );
  }

  return (
    <Notice tone="error" title="Planning failed">
      {apiErrorMessage(error, "The planner did not answer.")}
    </Notice>
  );
}
