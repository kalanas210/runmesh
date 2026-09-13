"use client";

import Link from "next/link";
import { useNow } from "@/lib/now";
import { useJobs } from "@/hooks/useJobs";
import { Button } from "@/components/ui/button";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorState } from "@/components/ui/error-state";
import { Freshness } from "@/components/ui/freshness";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/shell/page-header";
import { JobStatBand } from "@/components/jobs/job-stat-band";
import { JobTable } from "@/components/jobs/job-table";
import { HealthStrip } from "./health-strip";

/**
 * Is the runtime healthy, and what is running right now.
 *
 * Two questions, two sources, and they are kept visually apart because they are
 * about different populations: the health strip is this PROCESS answering for
 * itself, and everything below it is the most recent jobs — a cursor page, not
 * a census. See lib/job-stats.ts for why that distinction is laboured
 * everywhere it appears.
 *
 * NOTHING HERE IMPLIES A TREND. There is no throughput tile, no p95, no queue
 * depth over time, and no sparkline. The only fleet telemetry that exists is
 * the instantaneous /ready payload — `engine.Stats()` has no caller and
 * memstore's Snapshot has no pgstore counterpart — so every one of those would
 * be a line drawn through numbers nobody measured. A metrics endpoint now
 * exists for Prometheus to scrape; a console that scraped it once a second to
 * draw its own chart would be a worse Prometheus.
 *
 * The recent list is capped at ten rather than paged. An overview that grows a
 * pager is a job list, and there is already a job list.
 */
export function Overview() {
  const jobs = useJobs({ states: [], limit: 10 });
  const now = useNow();
  const rows = jobs.data?.jobs ?? [];

  return (
    <>
      <PageHeader
        kicker="Overview"
        title="What this runtime is doing"
        description="One process, answering for itself, plus the ten most recent jobs it accepted."
        freshness={
          <Freshness
            updatedAt={jobs.dataUpdatedAt || null}
            error={jobs.isError ? jobs.error : undefined}
          />
        }
        action={
          <Button href="/submit" size="sm" arrow>
            Plan a job
          </Button>
        }
      />

      <div className="mt-8">
        <HealthStrip />
      </div>

      <div className="mt-6">
        <JobStatBand jobs={rows} loading={jobs.isPending} />
      </div>

      <section className="mt-10">
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 className="kicker">Recent jobs</h2>
          <Link href="/jobs" className="ulink text-[0.72rem] text-muted">
            All jobs
          </Link>
        </div>

        <div className="mt-3">
          {jobs.isPending ? (
            <Skeleton className="h-64 w-full" />
          ) : jobs.isError ? (
            <ErrorState
              error={jobs.error}
              caption="the recent jobs"
              onRetry={() => void jobs.refetch()}
            />
          ) : rows.length === 0 ? (
            <EmptyState
              title="No jobs yet"
              hint="Nothing has been submitted to this runtime. Describe a goal in English and read the plan before it runs."
              action={
                <Button href="/submit" size="sm" arrow>
                  Plan a job
                </Button>
              }
            />
          ) : (
            <JobTable jobs={rows} stale={jobs.isPlaceholderData} />
          )}
        </div>
      </section>

      {/* The clock is read once here so the page has a single source for
          "now" — it is already subscribed for the freshness chip, and reading
          it again in a child would only add another subscriber to the same
          one-second interval. */}
      <p className="mt-6 text-[0.62rem] text-faint">
        {now === null ? " " : "Times are UTC throughout this console."}
      </p>
    </>
  );
}
