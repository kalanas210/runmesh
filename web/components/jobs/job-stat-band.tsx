"use client";

import { formatRate, summarisePage, type PageSummary } from "@/lib/job-stats";
import { useNow } from "@/lib/now";
import { useReady } from "@/hooks/useReady";
import type { JobResponse } from "@/lib/types";
import { StatTile } from "@/components/ui/stat-tile";

/**
 * The band above the job list: four numbers, from two different populations,
 * labelled as such.
 *
 * ---------------------------------------------------------------------------
 * THE TWO POPULATIONS, AND WHY THEY ARE NOT BLENDED.
 *
 * `queue depth` and `inflight` come from GET /api/v1/ready and describe the
 * WHOLE RUNTIME at this instant. `active`, `failed` and the failure rate are
 * computed from the rows on screen, because `GET /api/v1/jobs` is
 * cursor-paginated with no total count and there is no aggregate endpoint of
 * any kind — /api/v1/metrics is the process's own exposition (request counts,
 * latencies, worker gauges), not a job census.
 *
 * A band that printed both groups in one row of identical tiles would read as
 * one set of facts about one population, and the fleet half would be believed
 * about the page half. So the page-derived tiles say "of the N shown" in their
 * hint, every time, and the fleet tiles name the runtime. It is four extra
 * lines of type and it is the difference between a number and a claim.
 *
 * NOTHING HERE IMPLIES A TREND. No throughput, no p95, no queue depth over
 * time. Those need a time series and the only fleet telemetry that exists is
 * instantaneous — engine.Stats() has no caller and memstore's Snapshot has no
 * pgstore counterpart. A sparkline here would be drawn from numbers nobody
 * measured.
 * ---------------------------------------------------------------------------
 */
export function JobStatBand({
  jobs,
  loading = false,
}: {
  jobs: readonly JobResponse[];
  loading?: boolean;
}) {
  const ready = useReady();
  // Null on the server, so the window arithmetic below is skipped entirely
  // rather than producing a number from a clock the browser will disagree with.
  const now = useNow();

  const summary: PageSummary | null = now === null ? null : summarisePage(jobs, now);

  const scope = `of the ${jobs.length} ${jobs.length === 1 ? "job" : "jobs"} on this page`;

  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <StatTile
        label="queue depth"
        value={ready.data?.queue_depth ?? "—"}
        hint="steps claimable right now, across the whole runtime"
        loading={ready.isPending}
        tone={ready.data && ready.data.queue_depth > 0 ? "default" : "muted"}
      />
      <StatTile
        label="inflight"
        value={ready.data?.inflight ?? "—"}
        hint={
          ready.data
            ? `steps leased by this process, of ${ready.data.workers} workers`
            : "steps leased by this process"
        }
        loading={ready.isPending}
      />
      <StatTile
        label="active"
        value={summary?.active ?? "—"}
        hint={`queued or running, ${scope}`}
        loading={loading || summary === null}
      />
      <StatTile
        label="failure rate"
        value={formatRate(summary?.failureRate ?? null)}
        hint={
          summary === null || summary.terminal === 0
            ? `nothing has finished ${scope}`
            : `${summary.failed} of ${summary.terminal} finished ${scope} failed or timed out`
        }
        loading={loading || summary === null}
        tone={summary && summary.failed > 0 ? "warn" : "default"}
      />
    </div>
  );
}
