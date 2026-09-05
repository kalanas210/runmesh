# ADR 0001 — Execution is at-least-once

**Status:** Accepted · Week 1

## Context

RunMesh retries failed steps, re-queues steps whose worker died, and reconciles
state after a crash. Any of those mechanisms can cause a step to run twice: a
worker can finish a step and die before recording the result, and the runtime
cannot distinguish that from a worker that died before running it at all.

Exactly-once execution of an arbitrary side effect is not achievable without
cooperation from the thing being executed. Pretending otherwise pushes an
unsolvable problem into every future design discussion about leases and retries.

## Decision

RunMesh provides **at-least-once** execution.

> Every step may execute more than once. Tools must either be idempotent, or be
> guarded by an idempotency key enforced with a unique constraint on
> `(job_id, step_id, idempotency_key)`.

This is stated in the README, in the tool-authoring docs, and in the `Tool`
interface doc comment. It is a contract, not an implementation detail.

## Consequences

- Retry, lease expiry and crash recovery can all be built without a distributed
  transaction, because duplicate execution is an accepted outcome rather than a
  bug.
- Every tool must declare whether it is naturally idempotent. Tools with external
  side effects need an idempotency key.
- "Exactly-once" never appears in project documentation. Where end-to-end
  effective-once behaviour matters, it comes from the idempotency key, and that
  is said explicitly.

## Alternatives considered

- **Exactly-once via two-phase commit across the store and the tool.** Requires
  every tool to be a transaction participant. Not viable for HTTP calls, Python
  sandboxes, or Kubernetes Jobs.
- **At-most-once (never retry).** Removes the duplicate-execution problem and
  replaces it with silent data loss on any transient failure. Unacceptable for a
  runtime whose main claim is reliability.
