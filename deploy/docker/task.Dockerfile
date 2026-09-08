# The reference task image: RunMesh's container tool contract, implemented.
#
#   docker build -f deploy/docker/task.Dockerfile -t runmesh/task:dev .
#   kind load docker-image runmesh/task:dev --name runmesh
#
# This is what a step's pod runs. It is deliberately tiny and deliberately
# free of any RunMesh library: the contract is environment variables in and a
# marker line out, so an image in any language can satisfy it. See
# cmd/task/main.go.

FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w" \
        -o /out/task ./cmd/task

# scratch, not distroless: cmd/task imports only the standard library, so there
# is genuinely nothing to put in the image but the binary. A task image is the
# thing most worth keeping small — it is pulled once per node per attempt, and
# it is the blast radius when a tool is compromised.
FROM scratch

COPY --from=build /out/task /task

# 65532 is distroless' "nonroot" uid, used here so the same value works whether
# a pod's securityContext names it or the image supplies it. Week 4 makes
# runAsNonRoot mandatory in the pod spec; this image already satisfies it.
USER 65532:65532

ENTRYPOINT ["/task"]
