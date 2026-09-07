package pgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/kalanas210/runmesh/migrations"
)

// migrationLockKey is the advisory-lock key the migrator holds while it runs.
//
// The problem it solves is a rolling deploy: five replicas start at once, all
// five see the same pending migration, and all five try to run it. Advisory
// locks are the mechanism PostgreSQL provides for exactly this — cheap,
// session-scoped, released automatically if the process dies mid-migration
// (which is the case a lock TABLE would leave wedged).
//
// The value is arbitrary but must never change: it is a rendezvous point, and
// two versions of this binary with different keys would not exclude each other.
const migrationLockKey int64 = 0x52554e4d // "RUNM"

// advisoryNamespace is the first half of every TWO-argument advisory lock this
// package takes, so a RunMesh lock cannot collide with another application's in
// a shared database. The casts at the call sites are deliberate: the two-
// argument form is (int4, int4) and the one-argument form is (int8), and
// leaving the overload for the planner to guess at is the sort of thing that
// works until somebody adds a function.
const advisoryNamespace int32 = 0x52554e4d

// migration is one embedded .sql file.
type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// Migrate applies every pending migration and reports how many it ran.
//
// Three properties, each because the alternative fails quietly:
//
//   - It holds an advisory lock, so concurrent replicas serialise instead of
//     racing to create the same table.
//   - Each migration runs in its OWN transaction together with the row that
//     records it, so a failure half way through a sequence leaves the database
//     at a version that actually exists rather than at an unknown one.
//   - It verifies the checksum of every already-applied migration. Editing a
//     migration that has shipped is the single most common way a team ends up
//     with two databases that claim the same version and have different
//     schemas; here it is a startup error naming the file.
func Migrate(ctx context.Context, db *sql.DB, log *slog.Logger) (int, error) {
	if log == nil {
		log = slog.Default()
	}
	all, err := loadMigrations()
	if err != nil {
		return 0, err
	}

	// The lock is taken on a single dedicated connection: pg_advisory_lock is
	// session-scoped, and a pooled *sql.DB gives no guarantee that the unlock
	// lands on the same connection as the lock.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("pgstore: acquiring a connection for migration: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return 0, fmt.Errorf("pgstore: acquiring the migration lock: %w", err)
	}
	defer func() {
		// Best effort: closing the connection releases the lock anyway.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.ExecContext(ctx, createSchemaMigrations); err != nil {
		return 0, fmt.Errorf("pgstore: creating schema_migrations: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}

	ran := 0
	for _, m := range all {
		if got, ok := applied[m.version]; ok {
			if got != m.checksum {
				return ran, fmt.Errorf(
					"pgstore: migration %d (%s) has changed since it was applied "+
						"(recorded %s, file %s). A migration that has shipped is immutable: "+
						"add a new one instead",
					m.version, m.name, short(got), short(m.checksum))
			}
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return ran, err
		}
		log.Info("applied migration", "version", m.version, "name", m.name)
		ran++
	}
	return ran, nil
}

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER     PRIMARY KEY,
    name       TEXT        NOT NULL,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

func appliedMigrations(ctx context.Context, conn *sql.Conn) (map[int]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("pgstore: reading schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int]string)
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("pgstore: reading schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// applyMigration runs one file and records it in the SAME transaction. If the
// process dies between the two, neither happened.
func applyMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pgstore: migration %d: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("pgstore: migration %d (%s): %w", m.version, m.name, err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
		m.version, m.name, m.checksum)
	if err != nil {
		return fmt.Errorf("pgstore: recording migration %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("pgstore: committing migration %d: %w", m.version, err)
	}
	return nil
}

// loadMigrations reads the embedded files and sorts them by version.
//
// The version is parsed from the filename rather than taken from sort order,
// so "0009" and "0010" order correctly and a file that does not follow the
// convention is a hard error instead of a migration that silently never runs.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("pgstore: reading embedded migrations: %w", err)
	}

	var out []migration
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("pgstore: migrations %s and %s share version %d",
				other, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := migrations.FS.ReadFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("pgstore: reading %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  version,
			name:     e.Name(),
			sql:      string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("pgstore: no migrations are embedded")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// short truncates a checksum for an error message.
//
// It is a function rather than a slice expression because the value on the left
// comes from the DATABASE, not from the file: a row somebody edited by hand, or
// a truncated column, is exactly the situation this error exists to report, and
// panicking on it would replace the diagnosis with a stack trace.
func short(sum string) string {
	const n = 12
	if len(sum) <= n {
		return sum
	}
	return sum[:n]
}

func parseVersion(name string) (int, error) {
	digits, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("pgstore: migration %q must be named <version>_<name>.sql", name)
	}
	v, err := strconv.Atoi(digits)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("pgstore: migration %q has no positive integer version prefix", name)
	}
	return v, nil
}
