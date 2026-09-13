"use client";

import { cn } from "@/lib/cn";
import { apiErrorCode, apiErrorMessage, apiStatus } from "@/lib/api";
import { ApiError } from "@/lib/api";
import { surface } from "./card";
import { Button } from "./button";

/**
 * The failed-to-load card.
 *
 * It prefers the API's own message over anything written here, because the Go
 * side writes a lowercase sentence naming the parameter and its valid range
 * ("limit must be an integer in [1, 1000]") and that is better copy than a
 * generic fallback could be. `caption` is the noun phrase that completes the
 * fallback — "the most recent jobs", lowercase, no full stop — so it reads as
 * "Could not load the most recent jobs."
 *
 * The request id is shown when there is one. That is the entire point of the
 * X-Request-ID header: the same id is in the server's log line for that
 * request, so a reader who can see it can hand an operator one string that
 * finds the failure instead of a description of what they were doing.
 *
 * role="alert" rather than "status", so it is announced rather than waited for.
 */
export function ErrorState({
  error,
  caption,
  onRetry,
  className,
}: {
  error: unknown;
  /** A lowercase noun phrase: "the job", "the recent jobs", "the tool catalogue". */
  caption: string;
  onRetry?: () => void;
  className?: string;
}) {
  const status = apiStatus(error);
  const code = apiErrorCode(error);
  const requestId = error instanceof ApiError ? error.requestId : undefined;

  // A 401 or 403 is not a transient failure and a Retry button on one is a lie:
  // the same key will be refused the same way for as long as it is the key.
  const retryable = onRetry && status !== 401 && status !== 403 && status !== 404;

  return (
    <div role="alert" className={cn(surface, "p-6", className)}>
      <p className="text-[0.86rem] text-muted">
        {apiErrorMessage(error, `Could not load ${caption}.`)}
      </p>

      {(status || code || requestId) && (
        <p className="tnum mt-3 text-[0.7rem] text-faint">
          {[status ? `HTTP ${status}` : null, code, requestId]
            .filter(Boolean)
            .join("  ·  ")}
        </p>
      )}

      {retryable && (
        <div className="mt-5">
          <Button variant="outline" size="sm" onClick={onRetry}>
            Try again
          </Button>
        </div>
      )}
    </div>
  );
}
