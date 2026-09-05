# ADR 0006 — Readiness is a predicate, not a state

**Status:** Accepted · Week 1

## Context

A step with unsatisfied dependencies must not run. The obvious modelling is a
`BLOCKED` state, or a `pending_deps` counter decremented as each dependency
finishes. Both require somebody to *unblock* the step, which raises the
questions: who does it, when, and what happens if that write is lost?

## Decision

There is no BLOCKED state and no dependency counter. A step with unsatisfied
dependencies sits in `QUEUED`, and the claim query excludes it:

```sql
AND NOT EXISTS (SELECT 1 FROM job_steps d
                 WHERE d.job_id = s.job_id AND d.id = ANY(s.depends_on)
                   AND d.state <> 'SUCCEEDED')
```

`blocked_by` on the API is derived on read by the same rule.

## Consequences

- There is nothing to unblock, so there is no unblocking bug. A step becomes
  claimable the instant its last dependency succeeds, because that is what the
  query says — not because a write succeeded somewhere.
- No stored value can drift out of agreement with reality, so no reconciler is
  needed to recompute readiness.
- Only SUCCEEDED unblocks. A dependency that failed leaves its dependents
  permanently unclaimable, which is correct and is exactly why `markDoomed`
  exists to cancel them rather than let the job hang.
- The cost is a subquery per candidate step. At RunMesh's scale that is a
  partial index away from irrelevant, and it is measured in Week 4 rather than
  assumed.
- The same predicate serves three callers — `Claim`, `QueueDepth` and
  `blocked_by` — so admission control cannot drift away from what actually gets
  claimed.

## Alternatives considered

- **A `BLOCKED` state.** Requires a writer, and that writer is a second source
  of truth about the dependency graph.
- **A materialised `pending_deps` counter.** Fast, and wrong the first time an
  update is lost or applied twice. Under an explicitly at-least-once contract,
  a counter mutated by retryable work is a bug waiting for load.
