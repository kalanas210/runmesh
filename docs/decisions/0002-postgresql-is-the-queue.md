# ADR 0002 — PostgreSQL is the queue; Redis is deferred

**Status:** Accepted · implemented in Week 2 ([`internal/pgstore`](../../internal/pgstore))

## Context

RunMesh needs a durable work queue. The obvious reach is Redis, but the job
record itself must live in PostgreSQL for durability, relational integrity and
query-ability (the dashboard needs joins, filters and history).

Writing the job to PostgreSQL and then pushing it to Redis is a **dual write**.
A crash between the two loses work or duplicates it. Fixing that properly means
a transactional outbox plus a relay process — real complexity, added before
there is any load to justify it.

## Decision

PostgreSQL is both the durable state store and the queue, claimed with:

```sql
SELECT ... FROM job_steps s JOIN jobs j ON j.id = s.job_id
WHERE <readiness predicate>
ORDER BY j.priority DESC, j.created_at, j.id, s.id
LIMIT $2
FOR UPDATE OF j SKIP LOCKED
```

Claiming a step and transitioning its state happen in the same transaction, so
there is no window in which the two stores disagree — there is only one store.

As implemented, the locking clause names the JOB row rather than the step row;
that is a separate decision with its own reasoning in
[ADR 0008](0008-the-job-row-is-the-lock.md), and it does not change anything
above.

Redis is introduced in Phase 3, and only for roles it is actually better at:

- token-bucket rate limiting for external APIs
- pub/sub fan-out of WebSocket events across API replicas
- short-lived operational cache

Redis is never the source of truth for a job, and never permanent storage.

## Consequences

- Every state transition is transactional. No consistency reconciliation between
  two stores.
- Queue throughput is bounded by PostgreSQL. This is fine at the scale RunMesh
  targets, and it is measured rather than assumed — see `docs/benchmarks`.
- `SKIP LOCKED` requires PostgreSQL 9.5+. Assumed, and CI runs 17.

## Alternatives considered

- **Redis as queue + PostgreSQL as state.** Dual write. Rejected above.
- **Transactional outbox + relay.** Correct, and the right answer at high scale.
  Deferred: it is pure infrastructure work with no learning payoff before the
  single-store version is proven insufficient.
- **A dedicated broker (NATS, RabbitMQ, Kafka).** Another moving part to run,
  learn and deploy for an MVP that has not yet demonstrated a throughput problem.
