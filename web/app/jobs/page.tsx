import { Suspense } from "react";
import type { Metadata } from "next";
import { JobList } from "@/components/jobs/job-list";
import { Skeleton } from "@/components/ui/skeleton";

export const metadata: Metadata = { title: "Jobs" };

/**
 * The Suspense boundary is a requirement, not a nicety. `useSearchParams`
 * inside JobList opts the whole route into client-side rendering, and the App
 * Router refuses to build a page that reads it without a boundary — the error
 * is at build time and names this file, which is why the fallback lives here
 * rather than being wished away with `export const dynamic`.
 *
 * The fallback is the page header's shape and nothing else. A spinner over the
 * whole route would be replaced a few milliseconds later by content of a
 * different height; a header-shaped block is the part of this screen whose
 * geometry is genuinely known before the query string is read.
 */
export default function JobsPage() {
  return (
    <Suspense fallback={<JobsFallback />}>
      <JobList />
    </Suspense>
  );
}

function JobsFallback() {
  return (
    <div aria-busy aria-label="Loading the job list">
      <div className="border-b border-line pb-6">
        <p className="kicker">Jobs</p>
        <Skeleton className="mt-3 h-8 w-96 max-w-full" rounded="sm" />
      </div>
      <Skeleton className="mt-8 h-24 w-full" />
    </div>
  );
}
