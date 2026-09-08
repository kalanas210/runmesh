# 10. The plan asks; the operator grants

Date: 2026-09-08
Status: Accepted

## Context

Before Week 4, a tool's descriptor carried `CPU`, `Memory`, `Image` and
`Network`, and nothing read them. The engine built `tools.Input.Limits` from the
step's timeout and attempt budget alone, so every Kubernetes Job was created with
no resource limits at all — the descriptors said `500m` and the pods got the
node.

That is a bug, but fixing it by copying the descriptor's numbers into the pod
spec would have encoded something worse as a design: the thing being executed
decides how much of the machine it gets. From Week 5 the thing being executed is
a plan written by a language model, and "the plan chooses its own resource
limits" is a sentence that should not survive being said out loud.

The project plan puts it in one line: *the orchestrator should never allow a task
to override security policy arbitrarily.*

## Decision

Introduce `internal/policy` and route every limit through it.

```
descriptor.Limits   ── a REQUEST
policy.Config       ── the GRANT   (from the operator's environment)
        │
        ▼
effective = min(request, ceiling), defaulted, never widened
```

Three rules, and the asymmetry between them is the whole design:

1. **Resources clamp, silently.** A tool asking for 8 CPUs on a cluster whose
   ceiling is 1 gets 1. This is not an attack and not a misconfiguration — it is
   a portable descriptor meeting a smaller cluster — so the right answer is to
   run it smaller, not to refuse the plan.

2. **Capabilities refuse, loudly.** A denied tool, a container-only tool in an
   in-process deployment, a tool asking for egress where egress is switched off:
   all of these are terminal, classified errors with a message naming the switch.
   Silently downgrading a network tool to no network produces a connection
   timeout that reads like the remote host's fault.

3. **Malformed input is fatal.** `memory: "256 MB"` is not a quantity. Nobody can
   say what sandbox was intended, so guessing one is worse than refusing.

The same `policy.Sandbox` object is consulted in three places, and that is
deliberate: three call sites resolving policy three slightly different ways is
how a plan gets accepted by the API and refused by the worker — or, much worse,
accepted by both and executed with an envelope neither of them checked.

| Where | When | Why there |
|---|---|---|
| `httpapi.createJob` | submit | a refused tool is a 400 with a reason, before anything is persisted |
| `engine.invoke` | **every attempt** | a policy tightened after submission still binds the next attempt |
| `GET /api/v1/tools` | on read | the catalogue publishes the grant, so the Week-5 planner is not told it has resources the cluster will not give it |

## Consequences

**Policy is resolved per attempt, not per submission.** A step can sit behind a
retry backoff for minutes and behind a dead worker's lease for longer. Resolving
once at submission would mean a policy tightened in between binds nothing already
queued — which is exactly the population an operator tightening a policy is
worried about. The cost is one map lookup and three quantity comparisons per
attempt, against a step that is about to create a pod.

**A policy refusal is terminal, and spends one attempt.** Retrying it would burn
the whole budget re-asking a settled question and bury the message that says what
to fix.

**Denied tools stay in the catalogue, marked.** Omitting them would make a 400
saying `unknown_tool` the only evidence that `python_execute` exists but is
switched off — an answer that costs an afternoon, and that a planner cannot act
on at all.

**A bad policy fails the boot.** An unparseable ceiling, or a default above its
own ceiling, is a `policy.New` error and the process exits. A runtime whose
security envelope is neither what was configured nor what was intended should not
be running.

**Two ceilings are reused rather than duplicated.** `MaxTimeout` and
`MaxAttempts` come from the existing plan limits. A second, larger policy ceiling
would be dead configuration; a second, smaller one would silently contradict the
400 the API already returns.

## What this does not do

Policy is per-deployment, not per-caller. There is no "this API key may use
`python_execute` and that one may not", because scoped keys already exist and
adding a second, overlapping authorisation model is how both become untrustworthy.
When per-tenant limits are needed, the scope system is where they belong.
