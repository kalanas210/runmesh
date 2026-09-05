# ADR 0005 — The store owns every state transition

**Status:** Accepted · Week 1

## Context

Something has to decide when a step moves from RUNNING to SUCCEEDED, when a job
becomes FAILED, and what happens to a job's other steps when one of them dies.
The candidates were: the worker mutates state directly; a single owning
goroutine per job serialises its own state; or every transition goes through
the store as a guarded write.

The constraint that settles it is Week 2. Once PostgreSQL is underneath and
there are two replicas, a goroutine cannot own a row. A worker on node B can
complete a step of a job whose in-memory owner lives on node A, and there is no
invalidation path back to A.

## Decision

**The store is the single owner of state.** Workers report outcomes; they never
mutate. Every transition is a guarded compare-and-set inside one critical
section:

```
step.lease_id == lease.ID  AND  step.state == expected  AND  CanStep(from, to)
```

In Week 1 that critical section is one mutex. In Week 2 it is one
`UPDATE … WHERE …` whose affected-row count must be 1. The three failure modes
are discriminated so callers can branch on them:

| condition | error | what the caller does |
|---|---|---|
| row absent | `ErrNotFound` | give up |
| token matches, state wrong | `ErrConflict` | discard the result |
| token does not match | `ErrLeaseLost` | **write nothing at all** |

The job's own state is never assigned directly. It is derived by one pure
function, `rollup`, evaluated at the end of every mutating method.

## Consequences

- A worker that lost its lease cannot clobber the new holder's result. That is
  the zombie-writer problem, and the fencing token is the whole of the answer.
- Both error branches already exist and are already tested, so PostgreSQL's
  behaviour under real contention is not a new code path — it is the path that
  was always there.
- There is exactly one implementation of "what does this job look like now",
  rather than one per call site.
- `rollup` and `markDoomed` are pure, so job-state precedence is table-tested
  without constructing a store, and both lift verbatim into a SQL transaction.
- The cost: the store is doing more than persistence. It applies the failure
  policy and the rollup. That is a deliberate trade — it is the only place
  where doing so is atomic.

## Alternatives considered

- **Workers mutate the store directly.** Every worker then needs the rollup
  logic, and two workers finishing sibling steps concurrently can both compute
  a job state from a stale view.
- **A goroutine per job owning its own state.** Genuinely elegant on one node,
  and dead on arrival at two. Rejected explicitly rather than discovered later.
- **Event sourcing, with status as a projection.** The Week-6 waterfall would
  come free. Rejected for Week 1: it needs a projector, and `Claim` would have
  to bypass it, giving two implementations of one transition.
