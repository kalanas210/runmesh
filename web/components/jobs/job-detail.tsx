"use client";

import { useCallback, useState } from "react";
import Link from "next/link";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { apiStatus } from "@/lib/api";
import { formatDuration, formatInstant } from "@/lib/format";
import { useNow } from "@/lib/now";
import { TERMINAL, isCancelling } from "@/lib/state";
import { useCancelJob } from "@/hooks/useCancelJob";
import { useJobFeed } from "@/hooks/useJobFeed";
import { Button } from "@/components/ui/button";
import { CopyButton } from "@/components/ui/copy-button";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorState } from "@/components/ui/error-state";
import { Freshness, stalenessOpacity } from "@/components/ui/freshness";
import { KeyValue } from "@/components/ui/kv";
import { Notice } from "@/components/ui/notice";
import { Skeleton } from "@/components/ui/skeleton";
import { StatePill } from "@/components/ui/state-pill";
import { TransportChip, transportReason } from "@/components/ui/transport-chip";
import { PageHeader } from "@/components/shell/page-header";
import { Waterfall, type WaterfallSelection } from "@/components/waterfall/waterfall";
import { CancelJobDialog } from "@/components/job/cancel-dialog";
import { ErrorChip } from "@/components/job/error-chip";
import { EventLog } from "@/components/job/event-log";
import { StepInspector } from "@/components/job/step-inspector";
import { StepTable } from "@/components/job/step-table";

/**
 * One execution, understood.
 *
 * ---------------------------------------------------------------------------
 * WHAT MAKES THIS SCREEN DIFFERENT FROM A STATUS PAGE.
 *
 * The waterfall is the hero, and it is drawn from the EVENT STREAM rather than
 * from the job snapshot, because the snapshot cannot describe what happened —
 * only what is happening. A claim overwrites `scheduled_at`, nulls
 * `started_at`/`ended_at` and increments `attempt`; a release or a lease expiry
 * nulls them outright. So a step that failed twice and succeeded on the third
 * try has, in its row, exactly one clean set of timestamps. A chart built from
 * those draws a single tidy bar for the most interesting step on the page.
 *
 * Everything else here exists to answer the question the chart raises. The step
 * table is the same steps as rows, for a reader who arrived with a step id. The
 * timeline is what the runtime recorded, in its own order. The inspector is one
 * step in full, including the result — and the selection that ties all four
 * together lives in the URL, so it can be shared.
 * ---------------------------------------------------------------------------
 *
 * ALL FOUR SCREEN STATES, none of them a spinner over the page:
 *
 *   LOADING  the snapshot arrives before the events, so the moment it lands the
 *            chart already knows how many lanes there are and draws them empty.
 *            The skeleton is lane-shaped for that reason: the geometry is known,
 *            so nothing reflows when the events arrive.
 *   ERROR    404 is its own copy — a job id is a thing people type and evict —
 *            and 401/403 says the key rather than the job is the problem.
 *   EMPTY    a job with no step selected, and a job whose history was truncated,
 *            are both handled rather than rendered as a blank panel.
 *   STALE    the Freshness chip counts, the transport chip says which source is
 *            feeding the screen, and past a minute the content itself dims.
 */
export function JobDetail({ jobId }: { jobId: string }) {
  const router = useRouter();
  const pathname = usePathname();
  const params = useSearchParams();
  const now = useNow();

  const feed = useJobFeed(jobId);
  const cancel = useCancelJob(jobId);
  const [confirming, setConfirming] = useState(false);

  /* ------------------------------------------------- selection, in the URL */

  const stepParam = params.get("step");
  const attemptParam = Number(params.get("attempt"));
  const selection: WaterfallSelection | null = stepParam
    ? { stepId: stepParam, attempt: Number.isFinite(attemptParam) && attemptParam > 0 ? attemptParam : null }
    : null;

  const select = useCallback(
    (next: WaterfallSelection | null) => {
      const search = new URLSearchParams(params.toString());
      if (next) {
        search.set("step", next.stepId);
        if (next.attempt === null) search.delete("attempt");
        else search.set("attempt", String(next.attempt));
      } else {
        search.delete("step");
        search.delete("attempt");
      }
      // `replace` and `scroll: false`: arrowing down the lanes writes the URL on
      // every keystroke, and `push` would bury the page the reader came from
      // under forty history entries. Scrolling to the top on each would be
      // worse still — the chart is halfway down the page.
      const query = search.toString();
      router.replace(query ? `${pathname}?${query}` : pathname, { scroll: false });
    },
    [params, pathname, router],
  );

  /* ------------------------------------------------------------- branching */

  if (feed.isLoading) return <DetailSkeleton />;

  if (!feed.job) {
    const status = apiStatus(feed.error);
    return (
      <>
        <PageHeader kicker="Job" title="That job could not be loaded" />
        <div className="mt-8 max-w-2xl">
          {status === 404 ? (
            <EmptyState
              title="That job id does not exist"
              hint={
                <>
                  It may have been evicted — with the in-memory store a restart
                  loses every job — or the id may be a typo. The list shows what
                  this runtime currently holds.
                </>
              }
              action={
                <Button href="/jobs" size="sm" arrow>
                  Back to the job list
                </Button>
              }
            />
          ) : (
            <ErrorState error={feed.error} caption="this job" onRetry={feed.refetch} />
          )}
        </div>
      </>
    );
  }

  const job = feed.job;
  const cancelling = isCancelling(job.state, job.cancel_requested_at);
  const terminal = TERMINAL.has(job.state);
  const age = now !== null && feed.updatedAt ? now - feed.updatedAt : 0;
  const degraded = transportReason(feed.transport);

  return (
    <>
      <PageHeader
        kicker={
          <span className="flex flex-wrap items-center gap-3">
            <Link href="/jobs" className="ulink">
              Jobs
            </Link>
            <span className="tnum normal-case tracking-normal text-faint">{job.id}</span>
          </span>
        }
        title={job.name}
        freshness={
          <span className="flex flex-wrap items-center gap-2">
            <StatePill state={job.state} scope="job" cancelling={cancelling} />
            <Freshness
              updatedAt={feed.updatedAt}
              error={feed.error}
              // A finished job's data cannot go stale, so a ticking age on one
              // measures nothing. FINAL is also the signal that the poll has
              // correctly stopped.
              terminal={terminal}
            />
            <TransportChip transport={feed.transport} />
          </span>
        }
        action={
          !terminal && (
            <Button variant="danger" size="sm" onClick={() => setConfirming(true)}>
              {cancelling ? "Cancel again" : "Cancel job"}
            </Button>
          )
        }
      />

      {degraded && (
        // The degradation is visible rather than silent. Every plausible cause
        // is a real condition somebody can fix, and a console that hid it would
        // present a lagging right edge as a quiet runtime.
        <Notice tone="warn" className="mt-6">
          {degraded}
        </Notice>
      )}

      {cancelling && (
        <Notice tone="warn" className="mt-6" title="Cancellation requested">
          Requested {formatInstant(job.cancel_requested_at)}
          {job.cancel_reason === "step_failed"
            ? " by the runtime itself: a step failed under fail_fast."
            : " by an operator."}{" "}
          Steps already leased by a worker keep running until their next
          heartbeat, so the job honestly stays {job.state} while it drains.
        </Notice>
      )}

      <div
        className="mt-8 transition-opacity"
        style={{ opacity: stalenessOpacity(age), transitionDuration: "var(--dur-state)" }}
      >
        <KeyValue
          className="rounded-2xl border border-line bg-ink-2 p-5"
          columns={2}
          rows={[
            { label: "priority", value: job.priority },
            { label: "on step failure", value: job.on_step_failure },
            {
              label: "steps",
              value: `${job.steps.length} · ${job.steps.filter((s) => TERMINAL.has(s.state)).length} settled`,
            },
            {
              label: "duration",
              value:
                job.duration_ms === null
                  ? job.started_at
                    ? "still running"
                    : "not started"
                  : formatDuration(job.duration_ms),
            },
            { label: "created", value: formatInstant(job.created_at) },
            { label: "ended", when: job.ended_at !== null, value: formatInstant(job.ended_at) },
            {
              label: "version",
              // The ETag. Worth showing because it is the number that moves on
              // every write, which makes it the fastest way to tell whether a
              // job that looks stuck actually is.
              value: <span className="text-muted">{job.version}</span>,
            },
            { label: "id", value: <CopyButton value={job.id} label={job.id} /> },
          ]}
        />

        {job.error && (
          <div className="mt-4">
            <ErrorChip error={job.error} />
          </div>
        )}

        <section className="mt-8">
          <h2 className="sr-only">Execution waterfall</h2>
          <Waterfall
            job={job}
            events={feed.events}
            truncated={feed.truncated}
            oldestSeq={feed.oldestSeq}
            selected={selection}
            onSelect={select}
            onActivate={select}
          />
        </section>

        {/* The inspector is a column beside the chart on a wide screen and a
            block under it otherwise. Not a drawer: a drawer over a chart hides
            the thing the reader is comparing against. */}
        <div className="mt-8 grid gap-8 xl:grid-cols-[minmax(0,1fr)_24rem]">
          <div className="min-w-0 space-y-8">
            <section>
              <h2 className="kicker">Steps</h2>
              <StepTable
                className="mt-3"
                job={job}
                selected={selection?.stepId ?? null}
                onSelect={(stepId) =>
                  select(selection?.stepId === stepId ? null : { stepId, attempt: null })
                }
              />
            </section>

            <section>
              <h2 className="kicker">Timeline</h2>
              <div className="mt-3">
                <EventLog
                  events={feed.events}
                  truncated={feed.truncated}
                  oldestSeq={feed.oldestSeq}
                  onSelectStep={(stepId, attempt) => select({ stepId, attempt })}
                />
              </div>
            </section>
          </div>

          <StepInspector
            className="xl:sticky xl:top-6 xl:self-start"
            job={job}
            events={feed.events}
            selection={selection}
            onSelect={select}
            onClose={() => select(null)}
          />
        </div>
      </div>

      <CancelJobDialog
        job={job}
        open={confirming}
        busy={cancel.isPending}
        error={cancel.error}
        onConfirm={() =>
          cancel.mutate(undefined, {
            // Closed only on success. A refused cancel — a 409 because the job
            // settled while the dialog was open — has to be read, and closing
            // the dialog would leave the reader watching a pill that did not
            // change and no explanation anywhere.
            onSuccess: () => setConfirming(false),
          })
        }
        onClose={() => {
          cancel.reset();
          setConfirming(false);
        }}
      />
    </>
  );
}

/**
 * The loading state, shaped like the screen it becomes.
 *
 * A header block, a record card and a chart-height panel. Nothing here guesses
 * a row count, because the number of steps is not known until the snapshot
 * arrives — and the moment it does, the real chart draws its own empty lanes at
 * the right height, so this never has to.
 */
function DetailSkeleton() {
  return (
    <div aria-busy aria-label="Loading the job">
      <div className="border-b border-line pb-6">
        <p className="kicker">Job</p>
        <Skeleton className="mt-3 h-8 w-80 max-w-full" rounded="sm" />
      </div>
      <Skeleton className="mt-8 h-32 w-full" />
      <Skeleton className="mt-8 h-72 w-full" />
    </div>
  );
}
