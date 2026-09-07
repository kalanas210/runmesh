# ADR 0008 — The job row is the lock

**Status:** Accepted · Week 2

## Context

Every mutation in `internal/pgstore` touches two kinds of row: a `job_steps` row
(the transition) and the `jobs` row (the rollup, the cancel flag, the per-job
event counter). Two kinds of row means two locks, and two locks taken in
different orders by different code paths is a deadlock.

The orders each method *naturally* wants are opposite:

- **Claim** selects step rows — that is what the readiness predicate matches —
  and then writes the job row when the rollup moves the job to `RUNNING`.
  Steps, then job.
- **Finish** is handed a lease naming one step, needs the job's cancel flag to
  decide whether a retry is still legal, and bumps the job's event counter.
  Job, then step.

Under concurrency that is textbook ABBA: Claim holds a `QUEUED` step row and
waits for the job; Finish holds the job and waits for that same `QUEUED` step in
order to cancel it. PostgreSQL detects the cycle and aborts one transaction with
`40P01`, so it is not silent corruption — but it is a failure that appears only
under production concurrency, which is the class of bug that reaches production.

## Decision

**Every mutating path locks the `jobs` row first, and only then touches step
rows.** There is exactly one lock order in the codebase, so there is no cycle to
build.

Single-job methods take it directly:

```sql
SELECT ... FROM jobs WHERE id = $1 FOR UPDATE
```

Multi-job sweeps — `Claim` and `ExpireLeases` — take it through the join, and
lock the *job*, not the step:

```sql
SELECT s.job_id, s.id
  FROM job_steps s JOIN jobs j ON j.id = s.job_id
 WHERE <predicate>
 ORDER BY j.priority DESC, j.created_at, j.id, s.id
 LIMIT $2
 FOR UPDATE OF j SKIP LOCKED
```

`SKIP LOCKED` still does its job: a job another replica is writing is passed
over rather than waited on, so N claimers make N claimers' worth of progress.
The unit of exclusion is a job rather than a step.

With the job row held, each method loads the job and its steps, applies the pure
policy in `internal/jobstate` — the same functions `memstore` applies — and
writes back the rows whose `version` changed.

**One exception: `Heartbeat` takes no job lock.** It is the hottest write in the
system, once per running step every few seconds. It only extends a lease on a
step a worker already owns, which is a state `Claim` never selects and the
rollup never reads, and it acquires nothing else — so it cannot be half of a
cycle.

## Consequences

- Deadlock between RunMesh's own transactions is structurally impossible rather
  than merely unobserved. No retry-on-`40P01` loop exists, because there is
  nothing for it to catch.
- Two workers cannot claim two steps of the *same* job concurrently: the second
  claimer skips that job for this tick. Steps of *different* jobs are unaffected,
  and a job's own parallelism is unaffected — its steps are claimed together in
  one batch. The dispatcher's ticker covers the skipped tick.
- The transition rules are not reimplemented in SQL, so the in-memory store and
  the PostgreSQL store cannot drift apart on policy. They can only drift on the
  parts that are genuinely different, which is what `internal/storetest`
  exercises.
- `jobs.updated_at` does not advance on every heartbeat. It is informational,
  and both stores behave the same way, which matters more.
- Read-modify-write inside the lock is more round trips than one clever
  statement would be. A job holds at most `RUNMESH_MAX_STEPS` (100) steps and
  the queries are index lookups on the primary key; when it stops being cheap,
  `docs/benchmarks` is where that will show up.

## Alternatives considered

- **Lock step rows, retry on deadlock.** Correct, and standard. Rejected because
  "we deadlock sometimes and recover" is a property nobody can reason about at
  3 a.m., and because the retry path would be exercised only in production.
- **`SERIALIZABLE` isolation.** Would turn the anomaly into a serialisation
  failure — still a retry loop, on a workload whose conflicts are already
  eliminated by one row lock.
- **Express each transition as a single `UPDATE ... WHERE`.** Tempting, and it
  is how the guarded compare-and-set is described. It does not survive the
  rollup: deciding a job's state needs every one of its steps, and fail-fast
  propagation writes several of them. Splitting that across statements
  reintroduces exactly the interleavings the lock removes.
- **An advisory lock keyed on the job id.** Works, and avoids touching the row.
  Rejected because it is a second, invisible locking scheme that `\d` and
  `pg_locks` describe less clearly than a row lock, for no gain over a row that
  every one of these transactions writes anyway.
