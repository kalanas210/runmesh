# ADR 0007 — One dependency: the PostgreSQL driver

**Status:** Accepted · Week 2 · supersedes the Week-1 scope of
[ADR 0004](0004-standard-library-http.md)

## Context

Week 1 shipped with an empty `require` block, and ADR 0004 closed with the rule
that follows from that: *every dependency added later has to justify itself
against a repository that currently has none.* Week 2 needs PostgreSQL, so this
is that justification.

Go's standard library provides `database/sql`, which is the *interface* to a
database. It provides no driver. Something has to speak the PostgreSQL wire
protocol.

## Decision

Add `github.com/jackc/pgx/v5`, used through `database/sql` via its
`pgx/v5/stdlib` adapter. One direct requirement, and five indirect ones it pulls
in: `pgpassfile`, `pgservicefile`, `puddle/v2`, `golang.org/x/sync` and
`golang.org/x/text`. Counting the transitive ones out loud is the point — a
dependency budget that only counts the line you typed is not a budget.

Two containment rules keep the blast radius at one line:

- **The driver is imported in exactly one place** — a blank import in
  `internal/pgstore` — and named as a string, `sql.Open("pgx", dsn)`. No query,
  no type and no error path in the codebase mentions pgx.
- **Every value crosses the boundary through `database/sql`'s own interfaces.**
  `internal/pgstore/rows.go` implements `driver.Valuer` and `sql.Scanner` by
  hand, including the `text[]` codec for `depends_on`, rather than relying on
  pgx's type system. `pgstore.New` takes a `*sql.DB` the caller built.

Swapping the driver is therefore changing one string, not auditing every query.

## Consequences

- The headline claim changes from "zero third-party dependencies" to "one, and
  it is the database driver". That is still worth saying, and it is now
  worth *defending* rather than merely counting.
- The dependency is on the critical path for durability, so it is pinned, and
  `go mod tidy` is verified in CI.
- The `database/sql` layer costs something real: no `LISTEN`/`NOTIFY` without a
  connection outside the pool (see the note on `Store.ready` in
  `internal/pgstore`), and no `COPY`. Neither is needed at this scale, and both
  are reachable later by using pgx natively in one file.

## Alternatives considered

- **`lib/pq`.** The historical default, now in maintenance mode; pgx is where
  the work happens and is what a Go team would pick today.
- **pgx natively, with `pgxpool`.** Faster, and better at arrays and JSON.
  Rejected because it would put pgx types in every query and every scan, which
  is the opposite of the containment above — and because `database/sql`,
  transactions and connection pooling are explicitly what this stage of the
  project exists to learn.
- **Write the wire protocol.** Genuinely tractable: the startup handshake,
  SCRAM-SHA-256 (Go 1.24 moved `pbkdf2` into the standard library) and the
  extended query protocol are perhaps 1,500 lines. Rejected, and it was a close
  call. It would preserve the zero-dependency claim and be the most interesting
  code in the repository — but it would put an unproven driver underneath the
  one thing Week 2 exists to prove, which is that `internal/storetest` passes
  against *real* PostgreSQL semantics. A conformance failure would then have two
  possible causes instead of one. Writing a driver is a good project; it is not
  this project.
