import type { ApiErrorBody, Detail, ErrorEnvelope, PlannerTrace } from "./types";

/**
 * The typed client. Every request goes to the Next route handler at /api/rm,
 * never to the Go API directly.
 *
 * That is one decision answering three problems at once. The Go API has no
 * CORS layer and adding one is not free — a preflight carries no Authorization
 * header and Auth runs outside the mux, so a header-less OPTIONS would 401
 * before routing, and every useful response header (ETag, X-Request-ID,
 * Location, Retry-After, Idempotency-Replayed) would need explicit exposing.
 * The bearer key would also have to reach the browser to be sent from it, and
 * a key in a bundle is a published key. Proxying through a route handler
 * costs one file, needs zero Go changes, keeps the key server-side, and makes
 * the cheap ETag poll work without any of the above.
 *
 * ApexTick uses axios. This does not, because the SSE reader in lib/stream.ts
 * needs a streaming body and axios in the browser buffers the whole response
 * through XHR. Running two HTTP clients so one screen can stream would be
 * worse than running none, and fetch does everything here.
 */

/** The proxy mount. Not configurable: it is a route in this app, not a host. */
export const RM_BASE = "/api/rm";

/**
 * A failed API call, carrying enough to branch on without re-reading the body.
 *
 * The envelope is parsed eagerly at the throw site rather than lazily by each
 * caller, because a Response body can only be read once and a component that
 * wants both the code and the message would otherwise get one of them.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly body?: ErrorEnvelope;
  readonly requestId?: string;
  /** Seconds, from the Retry-After header on a 429. */
  readonly retryAfter?: number;

  constructor(
    status: number,
    message: string,
    body?: ErrorEnvelope,
    requestId?: string,
    retryAfter?: number,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
    this.requestId = requestId;
    this.retryAfter = retryAfter;
  }
}

export interface ApiRequest extends Omit<RequestInit, "body"> {
  /** Serialised as JSON. Pass nothing for a GET. */
  json?: unknown;
  /** Query parameters. undefined and "" are dropped so a query key stays stable. */
  query?: Record<string, string | number | boolean | undefined | null | string[]>;
}

function buildUrl(path: string, query?: ApiRequest["query"]): string {
  const url = `${RM_BASE}${path}`;
  if (!query) return url;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === "") continue;
    // `state` is repeatable on GET /jobs, so an array becomes repeated keys
    // rather than a comma-joined string the Go side would reject.
    if (Array.isArray(value)) {
      for (const v of value) if (v !== "") params.append(key, v);
    } else {
      params.append(key, String(value));
    }
  }
  const qs = params.toString();
  return qs ? `${url}?${qs}` : url;
}

/**
 * The raw call. Returns the Response so a caller can read ETag, or handle a
 * 304, or stream. Throws ApiError for any other non-2xx.
 *
 * 304 is returned rather than thrown: it is the SUCCESS case of the job poll.
 * The ETag is the job Version and exists specifically so a dashboard can ask
 * "has anything changed" for almost nothing, and turning that into an
 * exception would make the cheap path the noisy one.
 */
export async function apiFetch(path: string, init: ApiRequest = {}): Promise<Response> {
  const { json, query, headers, ...rest } = init;
  const merged = new Headers(headers);
  if (json !== undefined && !merged.has("Content-Type")) {
    merged.set("Content-Type", "application/json");
  }

  const response = await fetch(buildUrl(path, query), {
    ...rest,
    headers: merged,
    body: json === undefined ? undefined : JSON.stringify(json),
    // The proxy already forbids caching upstream; this stops the browser's own
    // HTTP cache from answering a poll with a stale body behind our back.
    cache: "no-store",
  });

  if (response.ok || response.status === 304) return response;
  throw await toApiError(response);
}

async function toApiError(response: Response): Promise<ApiError> {
  let body: ErrorEnvelope | undefined;
  try {
    const parsed: unknown = await response.json();
    if (parsed && typeof parsed === "object" && "error" in parsed) {
      body = parsed as ErrorEnvelope;
    }
  } catch {
    // A proxy failure, a gateway page, an empty 502 — not every non-2xx that
    // reaches the browser came from the Go error envelope, and a JSON parse
    // failure here must not mask the status the caller needs to branch on.
  }

  const retryAfterHeader = response.headers.get("Retry-After");
  const retryAfter = retryAfterHeader ? Number(retryAfterHeader) : undefined;

  return new ApiError(
    response.status,
    body?.error.message ?? `${response.status} ${response.statusText}`.trim(),
    body,
    body?.error.request_id ?? response.headers.get("X-Request-ID") ?? undefined,
    Number.isFinite(retryAfter) ? retryAfter : undefined,
  );
}

/** The common case: a 2xx with a JSON body. */
export async function apiJson<T>(path: string, init: ApiRequest = {}): Promise<T> {
  const response = await apiFetch(path, init);
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

/**
 * A conditional GET. `data` is undefined on a 304, which means "unchanged" and
 * not "gone" — a caller keeps whatever it already had.
 */
export interface Conditional<T> {
  status: number;
  etag: string | null;
  data?: T;
}

export async function apiConditional<T>(
  path: string,
  etag: string | null | undefined,
  init: ApiRequest = {},
): Promise<Conditional<T>> {
  const headers = new Headers(init.headers);
  if (etag) headers.set("If-None-Match", etag);
  const response = await apiFetch(path, { ...init, headers });
  const nextEtag = response.headers.get("ETag");
  if (response.status === 304) return { status: 304, etag: etag ?? nextEtag };
  return { status: response.status, etag: nextEtag, data: (await response.json()) as T };
}

/* -------------------------------------------------------------------------- */
/* Error readers. The same names ApexTick's lib/api.ts exports, so a component
   moved between the two products keeps working.                              */
/* -------------------------------------------------------------------------- */

export function apiStatus(error: unknown): number | undefined {
  return error instanceof ApiError ? error.status : undefined;
}

export function apiErrorBody(error: unknown): ApiErrorBody | undefined {
  return error instanceof ApiError ? error.body?.error : undefined;
}

/** The stable machine-readable code, e.g. "not_found" or "resource_exhausted". */
export function apiErrorCode(error: unknown): string | undefined {
  return apiErrorBody(error)?.code;
}

/**
 * A human message. The Go side writes a lowercase sentence naming the
 * parameter and its valid range, which is better copy than anything a generic
 * fallback could produce — so prefer it, and only fall back when there is no
 * envelope at all.
 */
export function apiErrorMessage(error: unknown, fallback = "Something went wrong."): string {
  const body = apiErrorBody(error);
  if (body?.message) return body.message;
  const status = apiStatus(error);
  if (status === 401) return "This console is not authorised to read the runtime.";
  if (status === 403) return "This API key does not carry the scope that call needs.";
  return fallback;
}

/**
 * Validation problems keyed by field path, ready to hang off an editor line.
 * Plan.Validate collects EVERY problem rather than the first — explicitly
 * because the plan author may be a language model — and the field path is
 * literally "steps[2].depends_on[0]", so a form can key straight off it.
 */
export function apiFieldErrors(error: unknown): Record<string, string> {
  const details: Detail[] = apiErrorBody(error)?.details ?? [];
  return Object.fromEntries(details.map((d) => [d.field, d.issue]));
}

export function apiDetails(error: unknown): Detail[] {
  return apiErrorBody(error)?.details ?? [];
}

/** The planner's 422 carries a trace beside the envelope. Nothing else does. */
export function apiTrace(error: unknown): PlannerTrace | undefined {
  return error instanceof ApiError ? error.body?.trace : undefined;
}

/** Seconds to wait, from a 429's Retry-After. */
export function apiRetryAfter(error: unknown): number | undefined {
  return error instanceof ApiError ? error.retryAfter : undefined;
}

/**
 * Retry predicate for React Query.
 *
 * The client default is `retry: 1`, which doubles up 401, 403 and 404 — three
 * answers that will not change on a second ask, and on an operator console
 * that means every missing job id is fetched twice. Anything below 500 is a
 * settled answer; only a server fault is worth asking again.
 */
export function retryOn5xx(failureCount: number, error: unknown): boolean {
  const status = apiStatus(error);
  if (status !== undefined && status < 500) return false;
  return failureCount < 1;
}
