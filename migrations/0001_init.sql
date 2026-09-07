-- 0001_init — the durable state store and the work queue.
--
-- One database, three tables, no broker. Claiming a step and transitioning it
-- happen in the same transaction, so there is no window in which two stores
-- disagree: there is only one store. See docs/decisions/0002.
--
-- Every column here has a field in internal/runmesh. The mapping is 1:1 and
-- deliberately boring — the interesting engineering is in the claim query and
-- the lock protocol, not in a clever schema.

-- ─── jobs ────────────────────────────────────────────────────────────────────

CREATE TABLE jobs (
    -- Text, not uuid. Ids are k-sortable (six bytes of millisecond timestamp,
    -- then ten random), so this primary-key b-tree APPENDS instead of
    -- fragmenting the way a random uuid v4 does, and `ORDER BY id DESC` is the
    -- whole of keyset pagination — no second column, no OFFSET.
    id                  TEXT        PRIMARY KEY,
    name                TEXT        NOT NULL,

    -- The eight lifecycle states, as text with a CHECK rather than an ENUM
    -- type. An ENUM would be a byte narrower and would reject typos at the same
    -- point; it also makes adding a state an ALTER TYPE that cannot run inside
    -- the same transaction as the code that needs it. Text plus CHECK keeps a
    -- future state a one-line migration.
    state               TEXT        NOT NULL
                        CHECK (state IN ('QUEUED','SCHEDULED','RUNNING','RETRYING',
                                         'SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')),
    priority            INTEGER     NOT NULL DEFAULT 0,
    on_step_failure     TEXT        NOT NULL DEFAULT 'fail_fast'
                        CHECK (on_step_failure IN ('fail_fast','continue_on_failure')),

    -- NULL rather than empty string when absent: the partial unique index below
    -- indexes only the rows that carry a key, so unlimited submissions without
    -- one do not collide with each other.
    idempotency_key     TEXT,

    -- A FLAG, not a state. The job stays RUNNING while in-flight steps drain;
    -- only the rollup moves it to a terminal state. This is what stops the
    -- store claiming a job is CANCELLED while a worker is still writing to one
    -- of its steps.
    cancel_requested_at TIMESTAMPTZ,
    cancel_reason       TEXT        NOT NULL DEFAULT '',

    error               JSONB,

    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    started_at          TIMESTAMPTZ,
    ended_at            TIMESTAMPTZ,

    version             BIGINT      NOT NULL DEFAULT 0,

    -- The per-job event counter, bumped by the same UPDATE that writes a
    -- transition. Keeping it on the job row rather than deriving MAX(seq) is
    -- what makes the sequence gap-free under concurrency: two appends cannot
    -- both read the same maximum and produce a duplicate or a hole in a cursor
    -- clients resume from.
    event_seq           BIGINT      NOT NULL DEFAULT 0
);

-- Partial, so only submissions that supplied a key participate.
CREATE UNIQUE INDEX jobs_idempotency_key_key
    ON jobs (idempotency_key) WHERE idempotency_key IS NOT NULL;

-- GET /api/v1/jobs?state=…&cursor=… : filter, then walk ids downwards.
CREATE INDEX jobs_state_id_idx ON jobs (state, id DESC);

-- The job half of the claim predicate. Partial, so the index holds only jobs
-- that could ever yield work — which on a mature installation is a tiny
-- fraction of the table.
CREATE INDEX jobs_claimable_idx ON jobs (priority DESC, created_at, id)
    WHERE cancel_requested_at IS NULL AND state IN ('QUEUED','RUNNING');

-- ─── job_steps ───────────────────────────────────────────────────────────────

CREATE TABLE job_steps (
    job_id           TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    -- Author-supplied and unique within the job, which is why the primary key
    -- is composite and there is no surrogate step id anywhere in the codebase.
    id               TEXT        NOT NULL,

    -- Plan order. The DAG decides execution order; this decides only the order
    -- steps are RENDERED in, so a submitted plan reads back the way it was
    -- written instead of in whatever order the index scan returned.
    ordinal          INTEGER     NOT NULL,

    tool             TEXT        NOT NULL,
    params           JSONB,

    -- An array rather than a join table. It is capped at RUNMESH_MAX_DEPENDS_ON
    -- (32) entries, and keeping it on the row is what lets the readiness
    -- predicate stay ONE self-contained NOT EXISTS instead of a second join —
    -- and the Go and SQL versions of that predicate stay legibly the same.
    depends_on       TEXT[]      NOT NULL DEFAULT '{}',

    state            TEXT        NOT NULL
                     CHECK (state IN ('QUEUED','SCHEDULED','RUNNING','RETRYING',
                                      'SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')),

    -- Nanoseconds, because that is what a time.Duration is. INTERVAL would read
    -- better in psql and worse in Go: every driver renders it differently, and
    -- a store that survives a driver swap is worth more than a pretty \d.
    timeout_ns       BIGINT      NOT NULL,

    max_attempts     INTEGER     NOT NULL,

    -- attempt is MONOTONIC: it increments on every claim and is never
    -- decremented, because it NAMES the execution (it becomes the Kubernetes
    -- Job name in Week 3). failures is the retry BUDGET and increments only
    -- when a real failure occurred. Conflating the two is how a rolling restart
    -- silently exhausts every in-flight job's retries.
    attempt          INTEGER     NOT NULL DEFAULT 0,
    failures         INTEGER     NOT NULL DEFAULT 0,

    -- The backoff IS this column. There is no sleeping goroutine per retrying
    -- step and no timer table: a step in RETRYING is simply not claimable until
    -- the clock passes this instant, so a million steps waiting out a backoff
    -- cost exactly nothing.
    next_attempt_at  TIMESTAMPTZ NOT NULL,

    -- The fencing token, regenerated on every claim. NULL means nobody holds
    -- this step; a worker presenting a token that does not match this one gets
    -- ErrLeaseLost and must write nothing at all.
    lease_id         TEXT,
    lease_owner      TEXT,
    lease_expires_at TIMESTAMPTZ,

    result           JSONB,
    error            JSONB,

    scheduled_at     TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    ended_at         TIMESTAMPTZ,
    version          BIGINT      NOT NULL DEFAULT 0,

    PRIMARY KEY (job_id, id)
);

-- The step half of the claim predicate: only ever scanned for steps that are
-- waiting to run, ordered so the backoff gate is the leading comparison.
CREATE INDEX job_steps_claimable_idx ON job_steps (next_attempt_at, job_id, id)
    WHERE state IN ('QUEUED','RETRYING');

-- The crash-recovery sweep: find leases whose holder stopped renewing them.
CREATE INDEX job_steps_lease_expiry_idx ON job_steps (lease_expires_at)
    WHERE state IN ('SCHEDULED','RUNNING');

-- ─── job_events ──────────────────────────────────────────────────────────────

CREATE TABLE job_events (
    -- Store-wide cursor for a dashboard-wide feed. A sequence, so it is
    -- allocated in insert order and never blocks a writer.
    --
    -- CAVEAT, stated here because it is easy to get wrong later: allocation
    -- order is not COMMIT order. A transaction that took global_seq 10 can
    -- become visible after one that took 11, so a live tail polling
    -- `global_seq > last` can step over an event that had not committed yet.
    -- That is harmless for Week 2 (nothing polls it) and is why Week 6's live
    -- feed is specified as LISTEN/NOTIFY push plus a per-job seq resume, not as
    -- a global-cursor poll.
    global_seq  BIGSERIAL   PRIMARY KEY,

    job_id      TEXT        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,

    -- Per-job, 1-based and gap-free: the cursor GET /jobs/{id}/events resumes
    -- from, and the WebSocket resume token. The UNIQUE constraint is not
    -- decoration — it is the assertion that the counter on the job row was
    -- bumped inside the same transaction as this insert.
    seq         BIGINT      NOT NULL CHECK (seq > 0),

    step_id     TEXT,
    attempt     INTEGER,
    type        TEXT        NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    state       TEXT,
    error       JSONB,
    duration_ms BIGINT,
    attrs       JSONB,

    UNIQUE (job_id, seq)
);
