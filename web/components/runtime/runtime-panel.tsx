"use client";

import { useTools } from "@/hooks/useTools";
import { useReady } from "@/hooks/useReady";
import { KeyValue } from "@/components/ui/kv";
import { Notice } from "@/components/ui/notice";
import { PageHeader } from "@/components/shell/page-header";
import { HealthStrip } from "@/components/overview/health-strip";

/**
 * What this process is configured to do, and — candidly — what it cannot yet
 * tell anyone.
 *
 * ---------------------------------------------------------------------------
 * THE "NOT YET INSTRUMENTED" PANEL IS THE POINT OF THIS SCREEN.
 *
 * Every dashboard has holes. The difference between a console an operator
 * trusts and one they learn to second-guess is whether the holes are labelled.
 * Each entry below is a fact about the API that this UI works around somewhere,
 * and naming them here means the workaround does not have to be rediscovered
 * from a chart that looks slightly wrong.
 *
 * Two of the gaps that were listed when this console was designed have since
 * been closed by the Go side, and they are recorded as closed rather than
 * quietly deleted: a reader who was told last month that there is no live
 * stream needs to see that there is one now.
 * ---------------------------------------------------------------------------
 */

interface Gap {
  title: string;
  detail: string;
  /** Where in this console the absence is visible. */
  shows: string;
}

const GAPS: Gap[] = [
  {
    title: "No per-step eligible-at timestamp",
    detail:
      "Nothing records when a step became claimable. NextAttemptAt is json:\"-\" and surfaces only while a step is RETRYING, so the left edge of every queued bar is derived from the job's creation and its dependencies' completion.",
    shows: "The waterfall and the step inspector both print the word “inferred” beside that number.",
  },
  {
    title: "No attempt history on the job row",
    detail:
      "A claim overwrites scheduled_at and nulls started_at and ended_at; a release or a lease expiry nulls them outright. The snapshot therefore describes the current attempt and nothing else.",
    shows:
      "Every attempt on this console is reconstructed from the event stream, which is why a truncated history is reported rather than smoothed over.",
  },
  {
    title: "No ordinal on the wire",
    detail:
      "A step's position in the plan reaches the client only as the order of the steps array. It cannot be re-derived after a sort.",
    shows: "The step table and the waterfall lanes are never sortable.",
  },
  {
    title: "No pod lifecycle events",
    detail:
      "POD_CREATED, POD_DELETED, TOOL_CALLED and STEP_OUTPUT_CHUNK are declared in the domain and have no producer anywhere in the runtime. tools.Output.Meta, which carries k8s_job and namespace, is discarded when a step settles.",
    shows:
      "The claimed-but-not-started gap is drawn as one interval; it cannot be split into scheduling versus image pull.",
  },
  {
    title: "No job aggregates",
    detail:
      "GET /api/v1/jobs is cursor-paginated with no total and there is no count-by-state endpoint. GET /api/v1/metrics is the process's own exposition — request counts, latencies, worker gauges — not a job census.",
    shows:
      "Every count above a job list says how many rows it was computed from, and no tile on this console implies a trend.",
  },
  {
    title: "No CORS on the API",
    detail:
      "Auth runs outside the mux and a preflight carries no Authorization header, so a header-less OPTIONS would be refused before routing. Five useful response headers would also each need exposing.",
    shows:
      "The browser never calls the API directly. Every request goes through this app's own route handler, which also keeps the key out of the bundle.",
  },
];

const CLOSED: Gap[] = [
  {
    title: "A live event stream now exists",
    detail:
      "GET /api/v1/jobs/{id}/stream is in the route table under jobs.read, with snapshot, event, resync, end and bye frames and resume on the per-job seq.",
    shows:
      "The job screen reads it and falls back to polling when it cannot, saying so in the chrome rather than degrading silently.",
  },
  {
    title: "A metrics endpoint now exists",
    detail:
      "GET /api/v1/metrics serves the Prometheus exposition format. It is scoped rather than public, and it is the process's own instrumentation.",
    shows:
      "Nothing on this console scrapes it. A dashboard that polled an exposition endpoint to draw its own charts would be a worse Prometheus.",
  },
];

export function RuntimePanel() {
  const ready = useReady();
  const tools = useTools();

  const denied = (tools.data?.tools ?? []).filter((tool) => tool.denied);
  const containerised = (tools.data?.tools ?? []).filter(
    (tool) => tool.execution === "container",
  ).length;

  return (
    <>
      <PageHeader
        kicker="Runtime"
        title="What this process is configured to do"
        description="Everything here is read from the running server. Nothing is a build-time constant."
      />

      <div className="mt-8">
        <HealthStrip />
      </div>

      <section className="mt-8 rounded-2xl border border-line bg-ink-2 p-6">
        <h2 className="kicker">Execution</h2>
        <KeyValue
          className="mt-4"
          columns={2}
          rows={[
            { label: "store", value: ready.data?.store ?? "…" },
            {
              label: "durability",
              value:
                ready.data === undefined
                  ? "…"
                  : ready.data.durable
                    ? "jobs survive a restart"
                    : "a restart loses every job",
            },
            { label: "workers", value: ready.data?.workers ?? "…" },
            {
              label: "tool execution",
              // Derived from the catalogue rather than from a config field,
              // because there is no config field on the wire: `execution` on a
              // descriptor is already the policy's resolved answer, and the
              // mode of the deployment is simply what it resolved to.
              value: tools.isPending
                ? "…"
                : containerised > 0
                  ? `${containerised} of ${tools.data?.tools.length ?? 0} tools run in a container`
                  : "every tool runs inside the server process",
            },
            {
              label: "tools refused",
              when: !tools.isPending,
              value:
                denied.length === 0 ? (
                  "none"
                ) : (
                  <span className="text-st-retrying">
                    {denied.map((tool) => tool.name).join(", ")}
                  </span>
                ),
            },
          ]}
        />

        {tools.data && containerised === 0 && (
          <Notice tone="warn" className="mt-5">
            Tools run in process here, so the CPU, memory and image limits in the
            catalogue are the policy&rsquo;s resolved values rather than
            constraints a sandbox is enforcing. They become real when the
            executor is switched to containers.
          </Notice>
        )}
      </section>

      <section className="mt-8">
        <h2 className="kicker">Not yet instrumented</h2>
        <p className="mt-2 max-w-2xl text-[0.78rem] leading-relaxed text-muted">
          Each of these is a thing this console works around. They are listed
          because an unlabelled hole in a dashboard is rediscovered later as a
          chart that looks slightly wrong.
        </p>

        <ul className="mt-4 space-y-3">
          {GAPS.map((gap) => (
            <li key={gap.title} className="rounded-2xl border border-line bg-ink-2 p-5">
              <h3 className="text-[0.85rem] text-bone">{gap.title}</h3>
              <p className="mt-1.5 text-[0.75rem] leading-relaxed text-muted">{gap.detail}</p>
              <p className="mt-2 text-[0.68rem] leading-snug text-faint">{gap.shows}</p>
            </li>
          ))}
        </ul>
      </section>

      <section className="mt-8">
        <h2 className="kicker">Closed since this console was designed</h2>
        <ul className="mt-4 space-y-3">
          {CLOSED.map((gap) => (
            <li
              key={gap.title}
              className="rounded-2xl border border-st-succeeded/30 bg-st-succeeded/[0.04] p-5"
            >
              <h3 className="text-[0.85rem] text-st-succeeded">{gap.title}</h3>
              <p className="mt-1.5 text-[0.75rem] leading-relaxed text-muted">{gap.detail}</p>
              <p className="mt-2 text-[0.68rem] leading-snug text-faint">{gap.shows}</p>
            </li>
          ))}
        </ul>
      </section>
    </>
  );
}
