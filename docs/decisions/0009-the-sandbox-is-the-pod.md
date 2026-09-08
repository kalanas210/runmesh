# 9. The sandbox is the pod, not the interpreter

Date: 2026-09-08
Status: Accepted

## Context

Week 4 adds `python_execute`: a tool that runs source code chosen by whoever
submitted the plan, and from Week 5 by a language model. The obvious question is
what stops that code doing something terrible.

There is a well-trodden answer that does not work. It looks like this:

```python
exec(code, {"__builtins__": {"len": len, "range": range}})
```

Strip the builtins, block `import`, walk the AST and reject dangerous nodes, or
reach for a library that does all three. Every version of this has been bypassed,
publicly and repeatedly, and the bypasses are one-liners. The canonical one needs
no imports and no builtins at all:

```python
().__class__.__base__.__subclasses__()
```

From there it is a short walk to a file object, and from a file object to
everything. `RestrictedPython` — the most serious attempt in the ecosystem — is
explicit that it is not a security boundary against untrusted code.

The deeper problem is not that any particular filter is weak. It is that a filter
which *mostly* works is worse than no filter, because it manufactures the belief
that the code was vetted. Somebody then writes "the interpreter is sandboxed" in
a README, and the next person builds on that sentence.

## Decision

**Do not sandbox inside the interpreter. Sandbox the process, with the kernel.**

The Python image ships an ordinary, unrestricted CPython. The step's code gets
real builtins, real imports and the whole standard library. What it does not get
is anything to do with them:

| Control | Where it is set | What it stops |
|---|---|---|
| `runAsNonRoot`, uid 65532 | pod + container securityContext | anything that requires root |
| `readOnlyRootFilesystem` | container securityContext | modifying the image, persisting a binary |
| `capabilities: drop: [ALL]` | container securityContext | every capability-gated syscall |
| `allowPrivilegeEscalation: false` | container securityContext | setuid binaries, file capabilities |
| `seccompProfile: RuntimeDefault` | pod + container | ~60 syscalls behind most kernel escapes |
| cpu / memory limits | resources | a fork bomb or an allocation loop taking the node |
| `ephemeral-storage` + emptyDir `sizeLimit` | resources + volume | filling the node's disk |
| default-deny `NetworkPolicy` | the CNI | exfiltration, SSRF, the metadata service |
| unbound ServiceAccount, token not mounted | pod spec + RBAC | reaching the Kubernetes API |
| pod deleted when the step ends | executor + TTL | any persistence at all |

The interpreter is assumed hostile. The kernel, the cgroup and the CNI are the
boundary, and every one of them is enforced by something outside the process.

## Consequences

**`python_execute` cannot run in-process, ever.** There is no pod when
`RUNMESH_EXECUTOR=local`, so there is no sandbox, so the tool is refused at
submission with `tool_not_sandboxed`. `tools.Container.Run` refuses again if
anything ever reaches it. Two mechanisms, because the thing on the other side is
arbitrary code in the API process — security principle 1 in the project plan.

**The submit-time validator deliberately does not inspect the source.** It checks
that `code` is present and that it fits in the environment block, and stops
there. A test — `TestPythonExecuteDoesNotPretendToVetTheSource` — fails if
somebody adds a content filter later, with a comment explaining why. The point is
not that filtering is useless; it is that a filter in the parameter validator
reads like a security boundary and is not one.

**Every control above must be verified, not assumed.** The pod fields are pinned
by table tests over `BuildJob`. The NetworkPolicy is the one that cannot be
tested in Go — it is enforced by the CNI or it is not enforced at all — so
`deploy/kind/verify-networkpolicy.sh` sends real packets from real pods, and
`TestNetworkPolicySelectorsMatchTheJobLabels` reads the shipped manifest so the
label in Go and the selector in YAML cannot drift apart. See ADR 0010.

**A step needing a third-party package is a new image, not a bigger one.** The
Python image has no `requirements.txt` on purpose: every package added to it runs
inside the sandbox with the step's data, and the first one invites the second. A
tool descriptor already carries its own `Image`, so numpy is a second image and a
second descriptor.

## Alternatives considered

**gVisor or Kata Containers.** A genuinely stronger boundary — a user-space
kernel or a real VM per pod — and the correct next step for a multi-tenant
deployment. Rejected for now because it is a `RuntimeClass` on the pod spec, which
means this decision does not have to be revisited to adopt it: the one field
changes, nothing else does.

**WebAssembly instead of a container.** A real sandbox with a real security
model, and much smaller. Rejected because it changes the tool contract from "any
image, any language" to "anything that compiles to WASI", and because the Python
story there is still awkward. The container contract is what makes an image in
any language a first-class citizen.

**No `python_execute` at all.** The most defensible option, and the plan's own
advice is to start with controlled tools rather than arbitrary execution. It is
rejected because arbitrary code execution *with a real boundary around it* is the
engineering problem this project exists to demonstrate — and the shipped default
is still closed: no image configured means the tool is not registered.
