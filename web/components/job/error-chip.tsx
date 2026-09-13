import { cn } from "@/lib/cn";
import type { ErrorInfo } from "@/lib/types";

/**
 * A step or job failure, rendered as the three facts an operator needs before
 * they read the message: what kind of failure, whether it can be retried, and
 * which attempt produced it.
 *
 * ---------------------------------------------------------------------------
 * THE CODE IS AN OPEN STRING AND AN UNKNOWN ONE IS SHOWN VERBATIM.
 *
 * The sixteen codes below are the ones the RUNTIME guarantees, and they are
 * worth expanding because "tool_not_sandboxed" is not a sentence anybody can
 * act on and "this tool must run in a container, and this deployment runs
 * tools in process" is. But a TOOL chooses its own codes — errors.go says so —
 * so the set is open, and a code this build has never heard of is printed as
 * it arrived rather than mapped to "unknown". Mapping it would destroy the one
 * piece of information the tool author put there on purpose.
 * ---------------------------------------------------------------------------
 *
 * `retryable` is shown as a word, not a colour, and it is the field most worth
 * showing: it is the difference between a step that will come back on its own
 * and one that is finished, and the retry budget (`failures`) only moves for
 * the first kind.
 */

/** internal/runmesh/errors.go:36-72, in source order, expanded into sentences. */
const CODE_NOTES: Record<string, string> = {
  step_timeout: "the tool ran past this step's timeout and was cancelled",
  cancelled: "the job was cancelled before or during this step",
  lease_lost: "another worker took the step while this one was executing it",
  workload_lost:
    "the pod running this attempt was taken away — deleted, evicted or preempted — so it runs again",
  tool_abandoned: "the tool returned neither a result nor an error",
  tool_panic: "the tool panicked",
  engine_panic: "the runtime itself panicked while running the step",
  unclassified: "the tool returned an error it did not classify, so it is terminal",
  tool_broke_contract: "the tool's output did not match its own contract",
  tool_unknown: "no tool of that name is registered in this deployment",
  output_too_large: "the result exceeded the tool's max output, and is not truncated",
  dependency_failed: "a step this one depends on failed, so it never ran",
  shutdown_drain: "the process was draining and gave the step back",
  tool_denied: "the execution policy refuses this tool in this deployment",
  tool_not_sandboxed: "this tool must run in a container, and tools here run in process",
  network_denied: "the tool asks for network egress and this deployment grants none",
  policy_violation: "the resolved sandbox is not one this deployment's policy permits",
};

export function ErrorChip({
  error,
  className,
}: {
  error: ErrorInfo;
  className?: string;
}) {
  const note = CODE_NOTES[error.code];

  return (
    <div
      className={cn(
        "rounded-xl border border-st-failed/40 bg-st-failed/[0.05] px-3.5 py-3",
        className,
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-[0.68rem] uppercase tracking-[0.14em] text-st-failed">
          {error.code}
        </span>
        <span
          className={cn(
            "rounded-full border px-2 py-0 font-mono text-[0.55rem] uppercase tracking-[0.14em]",
            error.retryable
              ? "border-st-retrying/50 text-st-retrying"
              : "border-line-2 text-muted",
          )}
        >
          {error.retryable ? "retryable" : "terminal"}
        </span>
        <span className="tnum text-[0.62rem] text-faint">attempt {error.attempt}</span>
      </div>

      {/* The tool's own sentence, and never truncated: on a failed step it is
          the whole reason the reader opened this panel. */}
      <p className="mt-2 break-words font-mono text-[0.72rem] leading-relaxed text-bone-2">
        {error.message}
      </p>

      {note && <p className="mt-1.5 text-[0.68rem] leading-snug text-faint">{note}</p>}
    </div>
  );
}
