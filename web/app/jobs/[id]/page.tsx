import { Suspense } from "react";
import type { Metadata } from "next";
import { JobDetail } from "@/components/jobs/job-detail";
import { Skeleton } from "@/components/ui/skeleton";

/**
 * The title is the job id and not the job's name, deliberately.
 *
 * A browser tab is read out of the corner of an eye, usually when several are
 * open at once, and two runs of the same plan have the same name. The id is the
 * thing that distinguishes them — and it is also what an operator pastes when
 * they ask somebody else to look.
 *
 * It comes from the route parameter rather than from a fetch: generating this
 * metadata by calling the API would double every job request, on the server,
 * with the server's own credentials, purely to improve a tab.
 */
export async function generateMetadata({
  params,
}: {
  params: Promise<{ id: string }>;
}): Promise<Metadata> {
  const { id } = await params;
  return { title: id };
}

export default async function JobPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;

  return (
    // JobDetail keeps its selection in the query string, so it reads
    // useSearchParams and the App Router requires the boundary.
    <Suspense fallback={<Loading />}>
      <JobDetail jobId={id} />
    </Suspense>
  );
}

function Loading() {
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
