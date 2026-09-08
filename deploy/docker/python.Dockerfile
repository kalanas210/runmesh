# The Python sandbox image: python_execute, implemented against the container
# tool contract.
#
#   docker build -f deploy/docker/python.Dockerfile -t runmesh/python:dev .
#   kind load docker-image runmesh/python:dev --name runmesh
#
# Then point the runtime at it:
#
#   RUNMESH_PYTHON_IMAGE=runmesh/python:dev
#
# The image is not the security boundary — the pod is. See the module docstring
# in deploy/docker/python/runner.py, and internal/k8s/job.go for the spec that
# actually confines this. What the image contributes is a small surface: no
# shell, no package manager, no compiler, no third-party packages, and a uid
# that is already non-root before any securityContext is applied.

FROM python:3.13-slim

# -B: never write .pyc files. The root filesystem is read-only in the pod, so a
# bytecode write would fail anyway; disabling it removes a per-start error and
# one reason to want a writable directory.
# -u: unbuffered. The result marker travels back through the pod's logs, and a
# buffered stdout on a container that exits promptly is how a result line goes
# missing on the fast path and appears on the slow one.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONHASHSEED=random \
    # The pod mounts an emptyDir here, and it is the only writable path.
    # Setting them in the image too means `docker run` behaves like the pod
    # does, which is what makes a local reproduction of a failure meaningful.
    HOME=/tmp \
    TMPDIR=/tmp

# Only the runner. No requirements.txt, on purpose: every package added here is
# code that runs inside the sandbox with the step's data, and the first one
# invites the second. A step that needs numpy is a reason to build a second
# image and name it in a tool descriptor — which the descriptor's Image field
# already supports — not a reason to widen this one.
COPY deploy/docker/python/runner.py /runner.py

# 65532 is distroless' "nonroot" uid, the same one the server and task images
# use. Declared in the image as well as enforced in the pod spec, so the two
# cannot disagree and so `docker run` reproduces the pod's identity.
USER 65532:65532

# -I is isolated mode: it ignores PYTHON* environment variables that would
# change import behaviour, does not put the script's directory on sys.path, and
# ignores the user site-packages directory. That last one matters here — a
# writable /tmp as HOME would otherwise make ~/.local/lib/python3.13/site-packages
# an import path a step could populate and then import from on the NEXT attempt
# if anything about the pod outlived it.
ENTRYPOINT ["python3", "-I", "-B", "-u", "/runner.py"]
