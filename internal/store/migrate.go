package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

// Migrations are compiled into the binary, so a fresh deployment needs nothing
// on disk but the executable. Hand-rolled rather than goose: the runner is
// forty lines and one fewer dependency (spec section 4.3).
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration is one schema step.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// migrationsTable is created before anything else and is never itself migrated.
const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    applied_at  INTEGER NOT NULL
)`

// LoadMigrations reads and parses the embedded migrations, sorted by version.
func LoadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %s and %s", version, prev, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, Migration{Version: version, Name: name, SQL: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName splits "0001_initial.sql" into 1 and "initial".
func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	num, name, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("migration %q: expected <version>_<name>.sql", filename)
	}
	version, err := strconv.Atoi(num)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: version %q is not a number: %w", filename, num, err)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("migration %q: version must be positive", filename)
	}
	return version, name, nil
}

// Migrate applies every migration not yet recorded, in version order. It is
// safe to run on every startup: already-applied migrations are skipped, so a
// second run is a no-op.
//
// Each migration and its bookkeeping row commit together, so an interrupted
// migration is never recorded as applied.
func (db *DB) Migrate(ctx context.Context) (applied []Migration, err error) {
	migrations, err := LoadMigrations()
	if err != nil {
		return nil, err
	}

	if _, err := db.write.ExecContext(ctx, migrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	current, err := db.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	for _, m := range migrations {
		if current[m.Version] {
			continue
		}
		if err := db.applyOne(ctx, m); err != nil {
			return applied, err
		}
		applied = append(applied, m)
	}
	return applied, nil
}

func (db *DB) applyOne(ctx context.Context, m Migration) error {
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.Version, m.Name, err)
		}
		// unixepoch() rather than a Go timestamp: this row is bookkeeping, not
		// domain state, and it must be written by the same statement batch that
		// applies the schema.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, unixepoch())`,
			m.Version, m.Name)
		if err != nil {
			return fmt.Errorf("record migration %04d_%s: %w", m.Version, m.Name, err)
		}
		return nil
	})
	return err
}

func (db *DB) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := db.write.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		out[v] = true
	}
	return out, rows.Err()
}

// SchemaVersion returns the highest applied migration version, or 0 if the
// database is empty.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := db.read.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		// A database that has never been migrated has no such table.
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}
