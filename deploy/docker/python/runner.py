"""RunMesh's Python tool image: the container tool contract, in Python.

It is the same contract cmd/task/main.go implements, deliberately reimplemented
in another language rather than shared through a library. That is the test of
whether the contract is really "environment variables in, one marker line out"
or secretly "whatever the Go binary happens to do".

    RUNMESH_TOOL             which behaviour to run
    RUNMESH_PARAMS           the step's params, as JSON
    RUNMESH_DEPS             upstream results keyed by step id, as JSON
    RUNMESH_JOB_ID           identity, for logging
    RUNMESH_STEP_ID
    RUNMESH_ATTEMPT
    RUNMESH_ATTEMPT_ID
    RUNMESH_IDEMPOTENCY_KEY  stable across attempts; what makes a retry safe
    RUNMESH_RESULT_MARKER    the sentinel to prefix the result line with

Exit 0 with one marker line on stdout means success. Any other exit code is a
terminal failure, which is the same default the in-process executor applies to
an unclassified error.


ON SANDBOXING, BECAUSE THIS IS THE FILE WHERE PEOPLE EXPECT IT

There is no attempt here to sandbox the user's code inside the interpreter. No
stripped __builtins__, no import auditing, no AST rewriting, no RestrictedPython.
Every one of those has been bypassed repeatedly and publicly; the bypasses are
one-liners, and a Python that "restricts eval" while a subclass walk from
().__class__.__base__.__subclasses__() reaches the file system is worse than no
restriction at all, because somebody will believe it.

The sandbox is the pod:

    a read-only root filesystem                 (nothing to modify or persist)
    runAsNonRoot, uid 65532, all caps dropped   (nothing to escalate to)
    allowPrivilegeEscalation: false             (no setuid path out)
    seccomp RuntimeDefault                      (the syscalls behind most escapes)
    cpu / memory / ephemeral-storage limits     (bounded blast radius)
    a NetworkPolicy that denies egress          (nothing to exfiltrate to)
    a ServiceAccount bound to nothing, unmounted (no cluster credentials)
    the pod is deleted when the step ends       (no persistence at all)

Assume the interpreter is hostile and the kernel is the boundary. That is the
only version of this that is true.
"""

import io
import json
import os
import sys
import traceback
from contextlib import redirect_stdout

DEFAULT_MARKER = "##RUNMESH-RESULT##"

# Well under any plausible log cap. The result travels back through the pod's
# logs, so a program that binds a gigabyte to `result` should fail here, with a
# sentence, rather than at the far end as a truncated line the parser rejects.
MAX_RESULT_BYTES = 256 * 1024

# Printed output is diagnostics, not the result. Keeping a bounded tail rather
# than the whole stream means a program that loops on print() still returns
# something useful instead of filling the node's disk through the log driver.
MAX_STDOUT_CHARS = 16 * 1024


def main() -> int:
    tool = os.environ.get("RUNMESH_TOOL", "")
    if not tool:
        print("runner: RUNMESH_TOOL is not set", file=sys.stderr)
        return 2

    marker = os.environ.get("RUNMESH_RESULT_MARKER") or DEFAULT_MARKER

    try:
        params = load_json("RUNMESH_PARAMS") or {}
        deps = load_json("RUNMESH_DEPS") or {}
    except ValueError as err:
        print(f"runner: {err}", file=sys.stderr)
        return 2

    print(
        "runner: tool={} job={} step={} attempt={}".format(
            tool,
            os.environ.get("RUNMESH_JOB_ID", ""),
            os.environ.get("RUNMESH_STEP_ID", ""),
            os.environ.get("RUNMESH_ATTEMPT", ""),
        )
    )

    if tool != "python_execute":
        print(f"runner: unknown tool {tool!r}", file=sys.stderr)
        return 2

    result, code = python_execute(params, deps)
    if code != 0:
        return code

    try:
        line = json.dumps(result, default=str)
    except (TypeError, ValueError) as err:
        # default=str already rescues most objects, so reaching here means
        # something genuinely unserialisable — a cycle, usually.
        print(f"runner: result is not JSON-serialisable: {err}", file=sys.stderr)
        return 2

    if len(line) > MAX_RESULT_BYTES:
        print(
            f"runner: result is {len(line)} bytes, limit is {MAX_RESULT_BYTES}",
            file=sys.stderr,
        )
        return 2

    print(f"{marker} {line}")
    return 0


def python_execute(params, deps):
    """Run the step's code and return (result, exit code).

    The program gets `params` and `deps` in its namespace and is expected to
    bind its answer to `result`. A convention, not a magic value: a function
    call would need a name to call, an expression would need the last statement
    to be one, and both make a multi-statement program awkward for the thing
    most likely to be writing it, which from Week 5 is a language model.
    """
    code = params.get("code")
    if not isinstance(code, str) or not code.strip():
        print("runner: python_execute needs a non-empty 'code' string", file=sys.stderr)
        return None, 2

    namespace = {
        "__name__": "__runmesh__",
        "params": params.get("params") or {},
        "deps": deps,
        "result": None,
    }

    # Compiled separately from exec so a syntax error is reported as one,
    # against a filename the traceback can name, instead of appearing as a
    # runtime failure inside the runner.
    try:
        compiled = compile(code, "<step>", "exec")
    except SyntaxError as err:
        print(f"runner: syntax error in the step's code: {err}", file=sys.stderr)
        return None, 2

    # The program's stdout is captured rather than interleaved with ours. The
    # result marker has to be the last line this process prints, and a program
    # that prints something marker-shaped must not be able to forge a result.
    buffer = io.StringIO()
    try:
        with redirect_stdout(buffer):
            exec(compiled, namespace)  # noqa: S102 - the pod is the sandbox
    except BaseException:  # noqa: BLE001 - including SystemExit and KeyboardInterrupt
        # The traceback goes to stderr, which becomes the pod's logs, which is
        # the only diagnostic that survives the pod being deleted. Exiting 1
        # rather than 2 keeps "the program failed" distinct from "the contract
        # was broken", which is what the exit code means to RunMesh.
        emit_captured(buffer)
        traceback.print_exc(file=sys.stderr)
        return None, 1

    emit_captured(buffer)

    result = namespace.get("result")
    if result is None:
        # Not an error. A program that only prints is a legitimate step, and
        # the contract says a missing result line is allowed — but returning
        # the captured output is far more useful than returning nothing, and it
        # is what makes `print(...)`-style code work at all for a caller that
        # did not read the schema.
        return {"stdout": tail(buffer.getvalue())}, 0
    return result, 0


def emit_captured(buffer):
    """Forward the program's own output to stderr, bounded."""
    text = tail(buffer.getvalue())
    if text:
        print(text, file=sys.stderr, end="" if text.endswith("\n") else "\n")


def tail(text: str) -> str:
    if len(text) <= MAX_STDOUT_CHARS:
        return text
    return "...[truncated]...\n" + text[-MAX_STDOUT_CHARS:]


def load_json(key: str):
    raw = os.environ.get(key)
    if not raw:
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError as err:
        raise ValueError(f"{key} is not valid JSON: {err}") from err


if __name__ == "__main__":
    sys.exit(main())
