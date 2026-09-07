package pgstore_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kalanas210/runmesh/internal/pgstore"
	"github.com/kalanas210/runmesh/internal/storetest"
)

// TestConformance is the point of the whole week.
//
// It runs internal/storetest — the suite memstore passes, byte for byte
// unmodified — against a real PostgreSQL. Week 1 asserted that the store
// interface was database-ready; every design says that about itself. This is
// where the claim either runs or does not.
//
// It needs a database. RUNMESH_TEST_DATABASE_URL points at one; without it the
// test skips rather than fails, so `go test ./...` still works on a laptop with
// no PostgreSQL, and CI — which always has one — is where the skip would be a
// lie. TestDatabaseIsConfiguredInCI below makes that lie impossible.
func TestConformance(t *testing.T) {
	dsn := testDSN(t)

	storetest.RunSuite(t, func(t *testing.T) storetest.Store {
		return newIsolatedStore(t, dsn)
	})
}

// TestMigrationsAreIdempotent: running the migrator twice must apply nothing
// the second time. A deploy that restarts a pod for any reason runs it again,
// and "applied on every boot" is how a team ends up afraid of their own
// migrations.
func TestMigrationsAreIdempotent(t *testing.T) {
	dsn := testDSN(t)
	db, _ := newIsolatedDB(t, dsn)

	first, err := pgstore.Migrate(t.Context(), db, nil)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if first == 0 {
		t.Fatal("the first run applied no migrations")
	}
	second, err := pgstore.Migrate(t.Context(), db, nil)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if second != 0 {
		t.Fatalf("the second run applied %d migrations; migrating must be idempotent", second)
	}
}

// TestMigrationChecksumIsEnforced: editing a migration that has already been
// applied is the single most common way two databases end up claiming the same
// version with different schemas. It must be a startup error, not a shrug.
func TestMigrationChecksumIsEnforced(t *testing.T) {
	dsn := testDSN(t)
	db, _ := newIsolatedDB(t, dsn)

	if _, err := pgstore.Migrate(t.Context(), db, nil); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Simulate somebody editing a shipped migration: the file's checksum no
	// longer matches what was recorded when it ran.
	if _, err := db.ExecContext(t.Context(),
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("rewriting the recorded checksum: %v", err)
	}

	_, err := pgstore.Migrate(t.Context(), db, nil)
	if err == nil {
		t.Fatal("Migrate accepted a migration whose checksum had changed")
	}
	if !strings.Contains(err.Error(), "has changed since it was applied") {
		t.Fatalf("error = %v, want it to name the changed migration", err)
	}
}

// TestDatabaseIsConfiguredInCI turns the skip above into a failure exactly
// where a skip would be dangerous. A conformance suite that silently skips is
// worse than no conformance suite: it reports green for a store nobody ran.
func TestDatabaseIsConfiguredInCI(t *testing.T) {
	if os.Getenv("CI") == "" {
		t.Skip("not running in CI")
	}
	if os.Getenv("RUNMESH_TEST_DATABASE_URL") == "" {
		t.Fatal("RUNMESH_TEST_DATABASE_URL is unset in CI: the PostgreSQL " +
			"conformance suite would have skipped and reported green")
	}
}

// ─── harness ─────────────────────────────────────────────────────────────────

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("RUNMESH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set RUNMESH_TEST_DATABASE_URL to run the PostgreSQL conformance suite, " +
			"e.g. postgres://postgres:postgres@127.0.0.1:5432/runmesh_test?sslmode=disable")
	}
	return dsn
}

// newIsolatedDB gives one subtest its own schema in the shared test database.
//
// A schema rather than a database: CREATE DATABASE copies a template and takes
// hundreds of milliseconds, and the suite runs its cases in parallel. A schema
// is a catalogue entry, and search_path makes every unqualified name in the
// migrations land inside it — so thirty parallel cases get thirty independent
// sets of tables without thirty database creations.
func newIsolatedDB(t *testing.T, dsn string) (*sql.DB, string) {
	t.Helper()

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generating a schema name: %v", err)
	}
	schema := "t_" + hex.EncodeToString(raw[:])

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening the admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("creating schema %s: %v", schema, err)
	}

	db, err := sql.Open("pgx", withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("opening %s: %v", schema, err)
	}
	// Small on purpose: the suite runs many cases in parallel, each with its own
	// pool, against a server whose default max_connections is 100.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	t.Cleanup(func() {
		_ = db.Close()
		drop, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer func() { _ = drop.Close() }()
		// Cleanup runs after t.Context() is already cancelled, so it needs a
		// deadline of its own; a DROP that hangs must not wedge the test binary.
		dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second) // clock:allow: post-test cleanup
		defer dcancel()
		_, _ = drop.ExecContext(dctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	return db, schema
}

func newIsolatedStore(t *testing.T, dsn string) storetest.Store {
	t.Helper()
	db, _ := newIsolatedDB(t, dsn)

	if _, err := pgstore.Migrate(t.Context(), db, nil); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	s, err := pgstore.New(db, pgstore.Options{})
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// withSearchPath appends the schema as a startup runtime parameter. Unknown
// connection-string keys are passed through to the server as GUCs, which is how
// a pool of connections all land in the same schema without a per-connection
// hook.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("RUNMESH_TEST_DATABASE_URL is not a URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
