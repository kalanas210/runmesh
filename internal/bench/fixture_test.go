package bench_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/kalanas210/runmesh/internal/clock"
	"github.com/kalanas210/runmesh/internal/memstore"
	"github.com/kalanas210/runmesh/internal/pgstore"
	"github.com/kalanas210/runmesh/internal/runmesh"
	"github.com/kalanas210/runmesh/internal/storetest"
	"github.com/kalanas210/runmesh/internal/tools"
)

// epoch is the fabricated "now" every store benchmark passes in. The stores
// take time as a parameter rather than reading a clock, which is what lets a
// benchmark drive a hundred thousand claims without a single real second
// passing and without a single timer firing underneath the measurement.
var epoch = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

const (
	benchLeaseTTL = 30 * time.Second
	// stepsPerJob is the shape every queueing benchmark uses. It is a
	// compromise with one specific failure in mind: memstore's Claim walks
	// every job and every step under one mutex, so a queue of one-step jobs
	// measures map iteration and a queue of one enormous job measures a linear
	// scan. Twenty-five is close to the largest DAG the shipped
	// RUNMESH_MAX_STEPS=100 admits in practice, and it keeps both stores
	// measuring the thing the name says.
	stepsPerJob = 25
)

// ─── stores ──────────────────────────────────────────────────────────────────

// backend names one store implementation and how to get an empty one. Every
// benchmark that can run against both is a table over this type, so a number
// for memstore and the matching number for pgstore come out of one command and
// cannot drift into different weeks.
type backend struct {
	name string
	open func(tb testing.TB) storetest.Store
}

// backends is memstore always, plus pgstore when a database was configured.
//
// The skip is the repository's existing convention, not a new one: the
// PostgreSQL cases in internal/pgstore skip on the same variable so that
// `go test ./...` still works on a laptop with no database, and CI — which
// always provides one — is where the skip would be a lie. A benchmark that
// silently measured only the in-memory store and reported green is the same
// failure TestDatabaseIsConfiguredInCI exists to prevent, so the pgstore rows
// SKIP loudly rather than disappearing.
func backends(tb testing.TB) []backend {
	tb.Helper()
	out := []backend{{
		name: "mem",
		open: func(tb testing.TB) storetest.Store { return memstore.New(memstore.Options{}) },
	}}
	if pgDSN() != "" {
		out = append(out, backend{name: "pg", open: newPGStore})
	}
	return out
}

// pgDSN reports the configured test database, or "". Reading the environment
// outside internal/config is a rule this repository states and means; a test
// fixture choosing whether it has a database to talk to is the exception the
// rule already carves out, and internal/pgstore's own harness reads the same
// variable the same way.
func pgDSN() string { return os.Getenv("RUNMESH_TEST_DATABASE_URL") }

// newPGStore gives one benchmark its own schema in the shared test database,
// migrated and empty.
//
// A schema rather than a database, for the reason internal/pgstore's own
// harness gives: CREATE DATABASE copies a template and costs hundreds of
// milliseconds, while a schema is a catalogue entry and search_path makes every
// unqualified name in the migrations land inside it. A benchmark that spends
// half a second on fixture creation per sub-case is a benchmark somebody stops
// running.
func newPGStore(tb testing.TB) storetest.Store {
	tb.Helper()

	dsn := pgDSN()
	if dsn == "" {
		tb.Skip("set RUNMESH_TEST_DATABASE_URL to benchmark the PostgreSQL store, " +
			"e.g. postgres://runmesh:runmesh@127.0.0.1:5434/runmesh?sslmode=disable")
	}

	var raw [8]byte
	for i := range raw {
		raw[i] = byte(rand.UintN(256))
	}
	schema := "b_" + hex.EncodeToString(raw[:])

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		tb.Fatalf("opening the admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		tb.Fatalf("creating schema %s: %v", schema, err)
	}

	db, err := sql.Open("pgx", withSearchPath(tb, dsn, schema))
	if err != nil {
		tb.Fatalf("opening %s: %v", schema, err)
	}
	// Deliberately generous, and deliberately stated: the contention benchmark
	// runs up to 32 claimers, and a pool smaller than the claimer count would
	// measure database/sql's connection queue instead of PostgreSQL's row
	// locks. The pgstore.Options doc makes the same point from the other side —
	// a pool that does not exceed the worker count can deadlock a drain.
	db.SetMaxOpenConns(48)
	db.SetMaxIdleConns(48)

	if _, err := pgstore.Migrate(context.Background(), db, quiet()); err != nil {
		tb.Fatalf("migrating %s: %v", schema, err)
	}
	s, err := pgstore.New(db, pgstore.Options{Log: quiet()})
	if err != nil {
		tb.Fatalf("pgstore.New: %v", err)
	}

	tb.Cleanup(func() {
		_ = s.Close()
		_ = db.Close()
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		// Cleanup needs a deadline of its own — a DROP that blocks behind a
		// connection the benchmark has not finished closing must not wedge the
		// test binary — and clock.WithWriteDeadline is how this codebase spells
		// a deadline that must survive whatever cancelled the caller.
		dctx, cancel := clock.WithWriteDeadline(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = drop.ExecContext(dctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	return s
}

// withSearchPath appends the schema as a startup runtime parameter, so every
// connection in the pool lands in the same schema without a per-connection hook.
func withSearchPath(tb testing.TB, dsn, schema string) string {
	tb.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatalf("RUNMESH_TEST_DATABASE_URL is not a URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ─── work ────────────────────────────────────────────────────────────────────

// nullExecutor is the narrowest tool that can exist: it allocates nothing,
// blocks on nothing and returns a constant.
//
// That is the point. The scheduler benchmark is trying to answer "how many
// steps per second can the dispatcher, the lease hand-off, the worker pool and
// the settle write move", and any tool with a body of its own puts its own cost
// inside that answer. tools.Local would add a registry lookup, a panic barrier,
// an output-size check and a json.Valid pass — all of which are real, all of
// which are measured separately, and none of which belongs in a number called
// steps/sec.
type nullExecutor struct{}

var nullResult = json.RawMessage(`{"ok":true}`)

func (nullExecutor) Execute(context.Context, tools.Input) (tools.Output, error) {
	return tools.Output{Result: nullResult}, nil
}

var _ tools.Executor = nullExecutor{}

// fill creates enough jobs of independent steps to make at least n steps
// claimable AS OF now, and returns how many it actually created.
//
// Independent, with no depends_on: a dependency chain would mean the queue
// drains one rank at a time and the benchmark would spend most of its time
// waiting for the readiness predicate rather than exercising the thing under
// test. Dependency gating is measured on its own, in the readiness benchmarks,
// where it is the subject rather than a confound.
//
// `now` is a parameter and not the package epoch, and the reason is a trap
// worth naming. Plan.Build stamps every step's NextAttemptAt with the timestamp
// it is given, and Claimable refuses a step whose NextAttemptAt is in the
// future. A store benchmark passes epoch and then passes the same epoch to
// Claim, so the two agree and the fabricated time never has to be real. An
// ENGINE benchmark cannot do that: the engine reads its own clock, so a queue
// stamped with a fabricated epoch is a queue that is either already claimable
// or permanently not, depending on which side of that constant the machine's
// wall clock happens to be on — and the failure is a dispatcher that claims
// nothing, forever, with no error anywhere.
func fill(tb testing.TB, s storetest.Store, n int, now time.Time) int {
	tb.Helper()

	jobs := (n + stepsPerJob - 1) / stepsPerJob
	total := 0
	for j := range jobs {
		p := &runmesh.Plan{
			Name:  "bench",
			Steps: make([]runmesh.PlanStep, 0, stepsPerJob),
		}
		for k := range stepsPerJob {
			p.Steps = append(p.Steps, runmesh.PlanStep{
				ID:     "s" + strconv.Itoa(k),
				Tool:   "echo",
				Params: nullResult,
			})
		}
		built := p.Build("job_bench_"+strconv.Itoa(j), now, runmesh.Defaults{
			StepTimeout: 30 * time.Second,
			MaxAttempts: 3,
		})
		if _, err := s.CreateJob(context.Background(), built, ""); err != nil {
			tb.Fatalf("CreateJob: %v", err)
		}
		total += stepsPerJob
	}
	return total
}

// claimN drains exactly n leases out of the store, one round trip per batch.
// It is setup for the benchmarks that measure Start, Heartbeat and Finish,
// which need a lease in hand before the timed section begins.
func claimN(tb testing.TB, s storetest.Store, n int) []runmesh.Lease {
	tb.Helper()

	out := make([]runmesh.Lease, 0, n)
	for len(out) < n {
		got, err := s.Claim(context.Background(), runmesh.ClaimRequest{
			Owner: "bench", Limit: min(64, n-len(out)), LeaseTTL: benchLeaseTTL, Now: epoch,
		})
		if err != nil {
			tb.Fatalf("Claim: %v", err)
		}
		if len(got) == 0 {
			tb.Fatalf("the queue ran dry after %d leases; fill() under-provisioned", len(out))
		}
		out = append(out, got...)
	}
	return out
}
