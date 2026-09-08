# The RunMesh server image.
#
#   docker build -f deploy/docker/server.Dockerfile -t runmesh/server:dev .
#
# Build context is the repository root, because the build needs go.mod and the
# whole module.

# ─── build ───────────────────────────────────────────────────────────────────
FROM golang:1.26 AS build

WORKDIR /src

# Dependencies first, as their own layer. They change far less often than the
# source does, so an ordinary code change reuses this layer instead of
# re-downloading client-go every time.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a static binary, which is what makes the scratch-like
# final stage possible at all. -trimpath keeps build paths out of the binary so
# the image is reproducible and does not leak the builder's directory layout.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/runmesh ./cmd/server

# ─── runtime ─────────────────────────────────────────────────────────────────
# distroless/static rather than alpine: no shell, no package manager, no libc,
# nothing to exploit and nothing to patch. The :nonroot tag runs as uid 65532,
# so the container cannot run as root even if the manifest forgets to say so.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /out/runmesh /runmesh

# Documentation only — the port comes from RUNMESH_HTTP_ADDR — but it is what
# `docker inspect` and most tooling read.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/runmesh"]
