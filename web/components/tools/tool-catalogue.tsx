"use client";

import { useMemo } from "react";
import { orderTools } from "@/lib/tools-policy";
import { useTools } from "@/hooks/useTools";
import { EmptyState } from "@/components/ui/empty-state";
import { ErrorState } from "@/components/ui/error-state";
import { Notice } from "@/components/ui/notice";
import { Skeleton } from "@/components/ui/skeleton";
import { PageHeader } from "@/components/shell/page-header";
import { ToolCard } from "./tool-card";

/**
 * What a plan may call here, and what it will actually be given.
 *
 * Usable tools first, denied ones last, alphabetically within each group. The
 * denied ones are present and findable rather than hidden, because the failure
 * they otherwise produce — `tool_denied` at submission time, or `unknown_tool`
 * if the registry never had it — sends a reader hunting for a typo in a name
 * that is spelled perfectly.
 *
 * There is no search box and no filter. Three tools ship by default and a
 * generously configured deployment has perhaps a dozen; a filter over twelve
 * cards is chrome nobody uses, and the browser's own find-in-page already works
 * because every card is real text.
 */
export function ToolCatalogue() {
  const tools = useTools();
  const ordered = useMemo(() => orderTools(tools.data?.tools ?? []), [tools.data]);
  const deniedCount = ordered.filter((tool) => tool.denied).length;

  return (
    <>
      <PageHeader
        kicker="Tools"
        title="The catalogue, as this deployment resolves it"
        description="Every entry is the effective descriptor: the execution mode and the resource limits have already been decided by the policy, not by the tool."
      />

      {deniedCount > 0 && (
        <Notice tone="warn" className="mt-6">
          {deniedCount} {deniedCount === 1 ? "tool is" : "tools are"} registered
          but refused by this deployment&rsquo;s execution policy. They are shown
          below with their reason, because a plan that names one is rejected at
          submission time and the name itself is not the problem.
        </Notice>
      )}

      <div className="mt-8">
        {tools.isPending ? (
          <div aria-busy aria-label="Loading the tool catalogue" className="grid gap-4 lg:grid-cols-2">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} className="h-64 w-full" />
            ))}
          </div>
        ) : tools.isError ? (
          <ErrorState
            error={tools.error}
            caption="the tool catalogue"
            onRetry={() => void tools.refetch()}
          />
        ) : ordered.length === 0 ? (
          /*
            Not reachable in any shipped configuration — builtin.go registers
            echo and sleep unconditionally and report.go adds report_generate —
            but implemented anyway, because "impossible" is a claim about the
            code as it is today and an empty grid with no explanation is a bug
            report nobody can act on.
          */
          <EmptyState
            title="This runtime has no tools registered"
            hint="Every deployment normally carries echo, sleep and report_generate. An empty catalogue means the registry was built without them, and no plan can be submitted until one is."
          />
        ) : (
          <div className="grid gap-4 lg:grid-cols-2">
            {ordered.map((descriptor) => (
              <ToolCard key={descriptor.name} descriptor={descriptor} />
            ))}
          </div>
        )}
      </div>
    </>
  );
}
