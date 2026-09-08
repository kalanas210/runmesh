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

# distroless/static rather than scratch, and the reason is one file.
#
# Week 3's version of this image WAS scratch: cmd/task imported only the
# standard library and there was genuinely nothing to add. Week 4 added
# http_request, and a static Go binary on scratch has no root certificates, so
# every https:// fetch fails with "certificate signed by unknown authority" —
# a message that sends you looking at the server, the proxy and the NetworkPolicy
# before you think of the image.
#
# distroless/static:nonroot is the smallest base that carries
# /etc/ssl/certs/ca-certificates.crt, and it still has no shell, no package
# manager and no libc. The image stays the blast radius when a tool is
# compromised, and it stays a few megabytes.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/task /task

# 65532 is distroless' "nonroot" uid, restated here so the same value works
# whether a pod's securityContext names it or the image supplies it. The pod
# spec sets runAsNonRoot, a read-only root filesystem and drops every
# capability (internal/k8s/job.go); this image already satisfies all of it.
USER 65532:65532

ENTRYPOINT ["/task"]
