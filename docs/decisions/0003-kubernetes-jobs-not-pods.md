# ADR 0003 — Execute tasks as Kubernetes Jobs, not raw Pods

**Status:** Accepted · applies from Week 3

## Context

RunMesh runs each task in an isolated Kubernetes workload. The two candidates
are a bare `Pod` and a `Job`.

## Decision

Use the `batch/v1` **Job** resource, configured as:

```yaml
spec:
  backoffLimit: 0            # retries belong to RunMesh, not Kubernetes
  ttlSecondsAfterFinished: 300
  template:
    spec:
      restartPolicy: Never
```

## Consequences

- Completion semantics come for free: a Job has a terminal `Succeeded`/`Failed`
  condition. A bare Pod requires RunMesh to interpret container statuses itself.
- Cleanup comes for free via `ttlSecondsAfterFinished`, so a crashed RunMesh
  process cannot leak workloads indefinitely. The reconciler is a safety net,
  not the only cleanup path.
- `backoffLimit: 0` is the important half of the decision. RunMesh owns the
  retry policy — attempt counts, backoff, and the retryable/terminal
  classification — and it owns the record of every attempt. If Kubernetes also
  retried, there would be two retry budgets, two sources of truth for attempt
  count, and an execution history the dashboard could not reconstruct.

  The division of labour is: **RunMesh owns retries, Kubernetes owns isolation
  and lifecycle.**
- `backoffLimit: 0` also means Kubernetes fails the Job for *any* pod that
  stops without succeeding, so a task that exited 1 and a pod that was deleted,
  evicted or preempted arrive as the same `BackoffLimitExceeded`. RunMesh tells
  them apart by asking the pod (added 2026-09-13, after `kubectl delete pod`
  was found to fail a step for good): positive evidence of removal — the pod
  gone, a deletion timestamp, `Evicted`, or a `DisruptionTarget` condition — is
  a retryable `workload_lost`. Anything else stays terminal, including an OOM
  kill, whose next attempt would meet the same limit, and a failure whose pod
  cannot be read at all.

## Alternatives considered

- **Raw Pods.** More direct control, but RunMesh reimplements completion
  detection and cleanup, and gets nothing in return.
- **Jobs with `backoffLimit > 0`.** Hands retry policy to Kubernetes, which
  cannot classify a 429 as retryable and a schema violation as terminal.
