"use client";

import { cn } from "@/lib/cn";
import { readyTone, useReady } from "@/hooks/useReady";
import { Notice } from "@/components/ui/notice";

/**
 * What the runtime says about itself, in one line.
 *
 * ---------------------------------------------------------------------------
 * `durable: false` IS THE MOST IMPORTANT FIELD ON THIS API AND IT IS SHOWN
 * FIRST AMONG EQUALS.
 *
 * The in-memory store answers 201 to a submission as though the job were safe,
 * and it is not: a restart loses every job, every result and every event. The
 * Go side added this field precisely because a status endpoint that overstates
 * durability is worse than no status endpoint — and a console that received it
 * and rendered it as a grey chip beside the worker count would be doing exactly
 * the overstating the field exists to prevent.
 *
 * So a non-durable store gets amber and a sentence, permanently, on the
 * overview. It is not a warning about something that went wrong; it is a
 * property of the deployment that the reader should know before they trust a
 * green board.
 * ---------------------------------------------------------------------------
 *
 * A 503 while draining is read as DATA rather than as an error — see
 * hooks/useReady.ts — because `ready` writes its body with writeJSON and not
 * with writeError, so the 503 carries a populated payload with the reason in
 * it. Turning that into a generic error card would discard the specific answer
 * at the moment it matters most.
 */
export function HealthStrip() {
  const ready = useReady();
  const tone = readyTone(ready.data);
  const data = ready.data;

  if (ready.isError) {
    return (
      <Notice tone="error" title="The runtime did not answer">
        GET /api/v1/ready failed, so nothing on this screen can be trusted to be
        current. The job list below is whatever was last fetched.
      </Notice>
    );
  }

  return (
    <div className="space-y-3">
      {tone === "draining" && (
        <Notice tone="warn" title="The runtime is draining">
          {data?.reason ?? "It is shutting down."} No new steps are being
          claimed. Jobs already in flight are finishing; anything queued will
          wait for the next process.
        </Notice>
      )}

      {data && !data.durable && (
        <Notice tone="warn" title="This runtime is not durable">
          Jobs are held in memory by the <span className="tnum">{data.store}</span>{" "}
          store. A restart loses every job, every result and every event —
          including the history the waterfall is drawn from.
        </Notice>
      )}

      <div className="flex flex-wrap items-center gap-x-6 gap-y-2 rounded-2xl border border-line bg-ink-2 px-5 py-3.5">
        <Field
          label="status"
          value={data?.status ?? "…"}
          tone={tone === "ok" ? "good" : tone === "draining" ? "warn" : "muted"}
        />
        <Field label="store" value={data?.store ?? "…"} />
        <Field
          label="durable"
          value={data === undefined ? "…" : data.durable ? "yes" : "no"}
          tone={data === undefined ? "muted" : data.durable ? "good" : "warn"}
        />
        <Field label="workers" value={data?.workers ?? "…"} />
        <Field label="inflight" value={data?.inflight ?? "…"} />
        <Field label="queue depth" value={data?.queue_depth ?? "…"} />

        <p className="ml-auto text-[0.62rem] text-faint">
          {/* Named, because every number here is a point reading and none of
              them is a rate. There is no time series anywhere on this API. */}
          instantaneous, polled every 5s
        </p>
      </div>
    </div>
  );
}

function Field({
  label,
  value,
  tone = "default",
}: {
  label: string;
  value: string | number;
  tone?: "default" | "good" | "warn" | "muted";
}) {
  return (
    <div>
      <p className="kicker text-[0.5rem]">{label}</p>
      <p
        className={cn(
          "tnum mt-0.5 text-[0.85rem]",
          tone === "good"
            ? "text-st-succeeded"
            : tone === "warn"
              ? "text-st-retrying"
              : tone === "muted"
                ? "text-faint"
                : "text-bone",
        )}
      >
        {value}
      </p>
    </div>
  );
}
