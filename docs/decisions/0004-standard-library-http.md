# ADR 0004 — Standard-library `net/http`, zero third-party dependencies in Week 1

**Status:** Accepted · Week 1

## Context

The default move for a Go HTTP service is Gin, Chi or Echo. RunMesh has two
reasons not to reach for one: the project exists partly to learn Go rather than
a framework, and Go 1.22 closed the gap that made routers necessary.

## Decision

Use `net/http` alone. `http.ServeMux` since Go 1.22 supports method and wildcard
patterns:

```go
mux.HandleFunc("POST /api/v1/jobs", h.createJob)
mux.HandleFunc("GET /api/v1/jobs/{id}", h.getJob)
```

Middleware is plain `func(http.Handler) http.Handler` composition. Structured
logging is `log/slog`. Week 1 has **no third-party module requirements at all** —
`go.mod` has an empty `require` block, and identifiers are generated from
`crypto/rand` rather than a UUID package.

## Consequences

- Nothing between the code and the standard library, which is the point when the
  goal is to be able to explain the code in an interview.
- A little more hand-written plumbing: middleware chain, JSON error envelope,
  request-scoped values. All of it is short, and all of it is the part worth
  understanding.
- Every dependency added later has to justify itself against a repository that
  currently has none.

## Alternatives considered

- **Chi.** Genuinely good and nearly stdlib-shaped. Rejected because the routing
  it adds is now in the standard library.
- **Gin.** Faster to write, but its context type propagates through the whole
  codebase and obscures exactly the `context.Context` plumbing this project is
  meant to demonstrate.
