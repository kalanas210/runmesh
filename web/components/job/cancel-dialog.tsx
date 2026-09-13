"use client";

import { apiErrorMessage, apiStatus } from "@/lib/api";
import type { JobResponse } from "@/lib/types";
import { ConfirmDialog } from "@/components/ui/confirm-dialog";
import { Notice } from "@/components/ui/notice";

/**
 * Stopping a job, and saying honestly what that does.
 *
 * ---------------------------------------------------------------------------
 * THE COPY IS THE POINT. CANCELLATION IS A REQUEST, NOT AN EVENT.
 *
 * `cancelJob` answers 202 and flags the job. Steps a worker already owns keep
 * running until their next heartbeat delivers the news, which may be on another
 * replica and may be seconds away. So "Cancel this job" in front of a reader
 * who then watches a step keep running for four seconds looks like a button
 * that did not work — and the reader's next move is to press it again, or to
 * go looking for a fault that is not there.
 *
 * Telling them beforehand costs two sentences and converts a confusing wait
 * into an expected one.
 * ---------------------------------------------------------------------------
 *
 * The failure branch keeps the dialog OPEN. A 409 here means the job settled
 * between the page rendering and the button being pressed, which is a fact the
 * reader needs; closing the dialog and leaving them to notice the pill did not
 * change would hide it behind the poll.
 */
export function CancelJobDialog({
  job,
  open,
  busy,
  error,
  onConfirm,
  onClose,
}: {
  job: JobResponse;
  open: boolean;
  busy: boolean;
  error: unknown;
  onConfirm: () => void;
  onClose: () => void;
}) {
  const leased = job.steps.filter(
    (step) => step.state === "RUNNING" || step.state === "SCHEDULED",
  ).length;

  return (
    <ConfirmDialog
      open={open}
      tone="danger"
      busy={busy}
      title="Cancel this job?"
      confirmLabel="Request cancellation"
      onConfirm={onConfirm}
      onCancel={onClose}
      body={
        <>
          <p>
            <span className="tnum text-bone-2">{job.name}</span> will stop
            scheduling new steps immediately.
          </p>
          <p className="mt-3">
            {leased > 0 ? (
              <>
                {leased} {leased === 1 ? "step is" : "steps are"} already leased by a
                worker and will keep running until their next heartbeat delivers
                the news. The job stays RUNNING while they drain, which is the
                honest answer rather than a race.
              </>
            ) : (
              <>
                No step currently holds a lease, so the job should settle on the
                next reconcile.
              </>
            )}
          </p>
          <p className="mt-3 text-faint">This cannot be undone. A cancelled job cannot be resumed.</p>

          {error && (
            <Notice tone="error" className="mt-4">
              {apiErrorMessage(error, "The cancel request was refused.")}
              {apiStatus(error) === 409 && (
                <> The job may have finished on its own while this dialog was open.</>
              )}
            </Notice>
          )}
        </>
      }
    />
  );
}
