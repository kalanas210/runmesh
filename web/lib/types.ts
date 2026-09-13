/**
 * The wire types, hand-transcribed from the Go source. Nothing here is guessed.
 *
 *   internal/httpapi/dto.go:20   jobResponse
 *   internal/httpapi/dto.go:38   stepResponse
 *   internal/httpapi/dto.go:65   errorInfo
 *   internal/httpapi/dto.go:72   jobListResponse
 *   internal/httpapi/dto.go:77   eventsResponse
 *   internal/httpapi/dto.go:84   toolsResponse
 *   internal/httpapi/dto.go:88   healthResponse
 *   internal/httpapi/dto.go:92   readyResponse
 *   internal/runmesh/event.go:11 EventType
 *   internal/runmesh/event.go:39 Event
 *   internal/runmesh/errors.go:36  the runtime error codes
 *   internal/runmesh/errors.go:112 ErrorInfo
 *   internal/runmesh/errors.go:133 Detail
 *   internal/runmesh/plan.go:15    Plan / PlanStep
 *   internal/tools/tool.go:67      Limits
 *   internal/tools/tool.go:88      Descriptor
 *   internal/planner/planner.go:98 Trace
 *
 * These are types, not a decoder. There is no runtime validation here on
 * purpose: the API is first-party, its DTOs are covered by Go tests, and a
 * hand-rolled schema check at the edge would be a second source of truth that
 * silently disagrees with the first one the moment a field is added.
 *
 * Four traps are encoded below rather than discovered later.
 *
 * 1. `timeout_seconds` is a FLOAT in SECONDS (dto.go:147, from Duration.Seconds()).
 *    Every other duration on this API is integer milliseconds. Multiplying the
 *    wrong one by 1000 is the easiest bug to ship here.
 * 2. Two different absence conventions in one API. jobResponse/stepResponse
 *    carry no omitempty, so an absent timestamp is an explicit `null`. Event
 *    IS omitempty, so `attempt` and `duration_ms` VANISH at zero rather than
 *    arriving as null — hence `?: number` on Event and `| null` on the others.
 * 3. `next_attempt_at` is populated ONLY while a step is RETRYING (dto.go:159).
 *    That is deliberate: the UI cannot render a backoff countdown for a step
 *    that is not waiting for one.
 * 4. `blocked_by` is DERIVED per request from Job.BlockedBy (dto.go:142) and is
 *    stored nowhere. It is valid only for the response that carried it, so it
 *    must not be cached across a poll.
 */

import type { JobState, StepState } from "./state";

/**
 * ErrorInfo. All four members are always present when the object is non-null —
 * there is no omitempty on the Go struct.
 *
 * `code` is an OPEN string. The sixteen values in KNOWN_ERROR_CODES are the
 * ones the runtime guarantees; a tool may return its own, and rendering an
 * unrecognised code verbatim is better than mapping it to "unknown".
 */
export interface ErrorInfo {
  code: string;
  message: string;
  retryable: boolean;
  attempt: number;
}

/** internal/runmesh/errors.go:36-67. Runtime and policy codes, in source order. */
export const KNOWN_ERROR_CODES = [
  "step_timeout",
  "cancelled",
  "lease_lost",
  "tool_abandoned",
  "tool_panic",
  "engine_panic",
  "unclassified",
  "tool_broke_contract",
  "tool_unknown",
  "output_too_large",
  "dependency_failed",
  "shutdown_drain",
  "tool_denied",
  "tool_not_sandboxed",
  "network_denied",
  "policy_violation",
] as const;

export type KnownErrorCode = (typeof KNOWN_ERROR_CODES)[number];

/** internal/runmesh/errors.go:133. The field path is literally "steps[2].depends_on[0]". */
export interface Detail {
  field: string;
  issue: string;
}

/** internal/runmesh/job.go:24-29, via FailurePolicy.String(). */
export type FailurePolicy = "fail_fast" | "continue_on_failure";

/** internal/runmesh/job.go:50-53. The two reasons a cancel can have. */
export type CancelReason = "user" | "step_failed";

/** internal/httpapi/dto.go:38. */
export interface StepResponse {
  id: string;
  tool: string;
  /** Never null: coerced to [] by orEmpty (dto.go:168). */
  depends_on: string[];
  /** Derived per request, never stored. Do not cache across a poll. */
  blocked_by: string[];

  state: StepState;
  /**
   * MONOTONIC, incremented on every claim, and the thing that names an
   * execution. NOT the retry budget — see `failures`. A step can legitimately
   * show attempt=4 with failures=1, because a release or a lease expiry claims
   * again without spending budget.
   */
  attempt: number;
  /** The retry BUDGET spent. Incremented only on a real failure. */
  failures: number;
  max_attempts: number;
  /** SECONDS, and a float. The one duration on this API that is not ms. */
  timeout_seconds: number;

  scheduled_at: string | null;
  started_at: string | null;
  ended_at: string | null;
  /** Non-null only while state === "RETRYING". */
  next_attempt_at: string | null;
  /**
   * ended_at - started_at, so TOOL EXECUTION time only. It excludes the
   * scheduled_at -> started_at gap, which under the Kubernetes executor is the
   * pod-pending wait and is often the interesting number. Do not draw a bar
   * from this and another bar from scheduled_at..ended_at: they will disagree.
   */
  duration_ms: number | null;

  /** Raw tool output. No schema; render it defensively. */
  result: unknown;
  error: ErrorInfo | null;
  version: number;
}

/** internal/httpapi/dto.go:20. */
export interface JobResponse {
  id: string;
  name: string;
  /** Only six of the eight states ever reach a job — see JOB_STATES. */
  state: JobState;
  priority: number;
  on_step_failure: FailurePolicy;
  version: number;
  created_at: string;
  updated_at: string;
  started_at: string | null;
  ended_at: string | null;
  duration_ms: number | null;
  /**
   * A cancelled job stays RUNNING while its leased steps drain, so
   * `cancel_requested_at != null && state === "RUNNING"` is a normal, correct
   * response. There is no CANCELLING state constant; the pill derives it.
   */
  cancel_requested_at: string | null;
  /** The only omitempty on the job: absent, not null, when unset. */
  cancel_reason?: CancelReason;
  error: ErrorInfo | null;
  steps: StepResponse[];
}

/** internal/httpapi/dto.go:72. `next_cursor` is omitempty: absent on the last page. */
export interface JobListResponse {
  jobs: JobResponse[];
  next_cursor?: string;
}

/** internal/runmesh/event.go:11-29. Fourteen constants; only ten have a producer. */
export const EVENT_TYPES = [
  "JOB_CREATED",
  "JOB_STARTED",
  "JOB_CANCEL_REQUESTED",
  "JOB_FINISHED",
  "STEP_SCHEDULED",
  "STEP_STARTED",
  "STEP_FINISHED",
  "STEP_RETRY_SCHEDULED",
  "STEP_RELEASED",
  "STEP_LEASE_EXPIRED",
  // Reserved with no producer anywhere in the repo. Declared so an exhaustive
  // switch can be written today, but a timeline that WAITS for TOOL_CALLED to
  // open a span, or for POD_CREATED to explain the scheduled gap, waits for ever.
  "POD_CREATED",
  "POD_DELETED",
  "TOOL_CALLED",
  "STEP_OUTPUT_CHUNK",
] as const;

export type EventType = (typeof EVENT_TYPES)[number];

/** The ten that are actually emitted. */
export const PRODUCED_EVENT_TYPES = EVENT_TYPES.slice(0, 10) as readonly EventType[];

/**
 * internal/runmesh/event.go:39, served VERBATIM on the wire — this is one of
 * the two places the domain type is not mapped through a DTO.
 *
 * `seq` is per-job, 1-based and gap-free: the cursor for ?after= and the stream
 * resume token. `global_seq` is store-wide. Resume on `seq`, never on
 * `global_seq` — under PostgreSQL the global allocation order is not commit
 * order, which the migration documents as a caveat.
 */
export interface RunMeshEvent {
  global_seq: number;
  seq: number;
  job_id: string;
  /** Absent on job-level events. */
  step_id?: string;
  /** omitempty: vanishes at 0, which is every job-level event and any step
   *  event before the first claim. Default it to 0, never require it. */
  attempt?: number;
  type: EventType;
  at: string;
  state?: StepState;
  error?: ErrorInfo;
  /** omitempty: vanishes at 0. Not `| null`. */
  duration_ms?: number;
  /**
   * Untyped and per-event-kind. There is no schema; narrow by `type` first.
   * Known shapes: JOB_CREATED {steps:number, name:string};
   * JOB_CANCEL_REQUESTED {reason:"user"} or {reason:"step_failed", step_id:string};
   * STEP_RETRY_SCHEDULED {next_attempt_at:string, failures:number};
   * STEP_RELEASED {reason:string};
   * STEP_LEASE_EXPIRED {owner:string, failures:number}.
   */
  attrs?: Record<string, unknown>;
}

/** internal/httpapi/dto.go:77. */
export interface EventsResponse {
  /** Never null: coerced to [] at jobs.go:223. */
  events: RunMeshEvent[];
  /** The resume cursor. Pre-seeded with the incoming `after`, so an empty page
   *  does not rewind the client. */
  next_after: number;
  /**
   * The cursor predates the oldest retained event. Real for the in-memory
   * ring, never for PostgreSQL. This is NOT cosmetic: it means the
   * event-reconstructed history has a permanent hole and the waterfall must
   * say so rather than draw a plausible, wrong chart.
   */
  truncated: boolean;
  oldest_seq: number;
}

/** internal/tools/tool.go:67. `Timeout` is json:"-" and is NOT on the wire. */
export interface ToolLimits {
  max_attempts: number;
  max_output_bytes: number;
  cpu?: string;
  memory?: string;
  ephemeral_storage?: string;
  image?: string;
  /** Always present, never omitempty. */
  network: boolean;
}

export type ExecutionMode = "in_process" | "container";

/**
 * internal/tools/tool.go:88, served verbatim.
 *
 * What arrives is the EFFECTIVE descriptor, not the tool's request:
 * policy.Engine.Effective has already overwritten execution and the resource
 * limits with what this deployment will actually grant. A denied tool is still
 * LISTED with denied:true — render it greyed with its reason, never hidden,
 * because omitting it makes "unknown_tool" the only evidence it exists.
 */
export interface ToolDescriptor {
  name: string;
  version: string;
  description: string;
  /** Raw JSON Schema, passed through untouched. */
  input_schema: unknown;
  limits: ToolLimits;
  execution: ExecutionMode;
  denied?: boolean;
  denied_reason?: string;
}

export interface ToolsResponse {
  tools: ToolDescriptor[];
}

/** internal/httpapi/dto.go:88. */
export interface HealthResponse {
  status: string;
}

/**
 * internal/httpapi/dto.go:92.
 *
 * These are INSTANTANEOUS values and the only fleet telemetry that exists —
 * there is no metrics route, engine.Stats() has no caller, and memstore's
 * Snapshot has no pgstore counterpart. Any tile implying a trend would be
 * fabricated.
 */
export interface ReadyResponse {
  status: string;
  store: string;
  /** The honest bit: with the in-memory store a restart loses every job. */
  durable: boolean;
  workers: number;
  inflight: number;
  queue_depth: number;
  /** Present when draining, alongside a 503. */
  reason?: string;
}

/** internal/runmesh/plan.go:15-30. */
export interface PlanStep {
  id: string;
  tool: string;
  params?: unknown;
  depends_on?: string[];
  timeout_seconds?: number;
  max_attempts?: number;
}

export interface Plan {
  name: string;
  priority?: number;
  on_step_failure?: FailurePolicy;
  steps: PlanStep[];
}

/** internal/planner/planner.go:111. One round trip to the model, and its verdict. */
export interface PlannerAttempt {
  n: number;
  input_tokens: number;
  output_tokens: number;
  accepted: boolean;
  problems?: Detail[];
  /** A failure that was not the plan's content: the API refused, the response
   *  was truncated, the JSON did not parse. */
  error?: string;
}

/**
 * internal/planner/planner.go:98.
 *
 * `reasoning` is populated ONLY on the accepted attempt, so a 422 trace has
 * none — a panel that expects it will render an empty box on exactly the
 * responses a reader opened it for.
 */
export interface PlannerTrace {
  model: string;
  attempts: PlannerAttempt[];
  reasoning?: string;
  /** The catalogue the model was actually offered, after policy removed what
   *  this deployment refuses. */
  tools_offered: string[];
  duration_ms: number;
  total_tokens: number;
}

export interface PlanResponse {
  plan: Plan | null;
  trace: PlannerTrace;
}

export interface GoalResponse {
  job: JobResponse;
  plan: Plan | null;
  trace: PlannerTrace;
}

/** internal/httpapi/errors.go:16-34. */
export const API_ERROR_CODES = [
  "invalid_argument",
  "unauthenticated",
  "permission_denied",
  "not_found",
  "failed_precondition",
  "resource_exhausted",
  "internal",
  "unprocessable",
  "unimplemented",
] as const;

export type ApiErrorCode = (typeof API_ERROR_CODES)[number];

/** internal/httpapi/errors.go:38-47. The single envelope every non-2xx body uses. */
export interface ApiErrorBody {
  code: string;
  message: string;
  details?: Detail[];
  request_id?: string;
}

/**
 * internal/httpapi/plans.go:199. The planner's 422 bypasses writeError and
 * writes the standard envelope PLUS a sibling top-level `trace` key. Every
 * error type on the client has to tolerate that extra key, which is why this
 * is one interface with an optional member rather than two.
 */
export interface ErrorEnvelope {
  error: ApiErrorBody;
  trace?: PlannerTrace;
}
