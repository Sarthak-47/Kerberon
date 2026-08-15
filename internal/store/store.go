// Package store owns all SQLite access: connection management, migrations and
// queries.
//
// Writes go through a single connection. WAL permits concurrent readers
// alongside exactly one writer, and Kerberon has many write paths — ingest,
// dispatch workers, the ack handler, the heartbeat sweeper, the UI. Letting
// them contend for the SQLite lock turns contention into multi-second stalls
// via busy_timeout. Capping the write pool at one connection makes them queue
// in Go instead, which is easier to reason about and removes a class of flaky
// test. See docs/DECISIONS.md D2.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver; CGO would break static builds
)

// driverName is the name modernc.org/sqlite registers itself under.
const driverName = "sqlite"

// DB is a handle to the Kerberon database. It holds two pools over the same
// file: a single-connection writer and a multi-connection reader.
type DB struct {
	write *sql.DB
	read  *sql.DB
	path  string
}

// Options tunes Open. The zero value is valid.
type Options struct {
	// ReadConns caps the reader pool. Defaults to GOMAXPROCS, minimum 2.
	ReadConns int
	// BusyTimeout is how long SQLite waits on a locked database. Defaults to
	// 5s per spec section 4.3. It should rarely be hit, since writes are already
	// serialized in Go.
	BusyTimeout int
}

func (o Options) withDefaults() Options {
	if o.ReadConns <= 0 {
		o.ReadConns = runtime.GOMAXPROCS(0)
	}
	if o.ReadConns < 2 {
		o.ReadConns = 2
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5000
	}
	return o
}

// Open opens (creating if absent) the database at path and verifies that the
// required pragmas actually took effect. It does not run migrations; call
// Migrate for that.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	opts = opts.withDefaults()

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path %q: %w", path, err)
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create database directory %q: %w", dir, err)
		}
	}

	// The writer uses BEGIN IMMEDIATE so a transaction that reads before it
	// writes cannot fail on lock upgrade partway through.
	write, err := openPool(ctx, abs, opts, true)
	if err != nil {
		return nil, err
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)

	read, err := openPool(ctx, abs, opts, false)
	if err != nil {
		_ = write.Close()
		return nil, err
	}
	read.SetMaxOpenConns(opts.ReadConns)
	read.SetMaxIdleConns(opts.ReadConns)

	db := &DB{write: write, read: read, path: abs}

	// Assert rather than assume. A pragma supplied in a DSN that the driver
	// silently ignores looks identical to one that worked, and the failure
	// would only surface later as corruption or as stalls under load.
	if err := db.verifyPragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func openPool(ctx context.Context, absPath string, opts Options, writer bool) (*sql.DB, error) {
	dsn := buildDSN(absPath, opts, writer)
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", absPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to sqlite %q: %w", absPath, err)
	}
	return db, nil
}

// buildDSN produces a SQLite URI. The URI form is required for _pragma
// parameters to be honoured at all; without the "file:" scheme the entire
// string, query included, is treated as a filename.
func buildDSN(absPath string, opts Options, writer bool) string {
	p := filepath.ToSlash(absPath)
	// A URI path must be escaped. Spaces are the case that actually occurs
	// (Windows user and project directories), and '?' or '#' would otherwise
	// terminate the path.
	r := strings.NewReplacer(" ", "%20", "?", "%3f", "#", "%23")
	p = r.Replace(p)
	if !strings.HasPrefix(p, "/") {
		// Windows drive letters: file:/D:/path is the accepted spelling.
		p = "/" + p
	}

	params := []string{
		// WAL gives concurrent readers alongside the single writer. Persisted
		// in the database file, but set on every connection so a fresh file
		// gets it too.
		"_pragma=journal_mode(WAL)",
		// NORMAL does not fsync on every commit. It remains crash-safe against
		// process death, which is what the chaos suite exercises; only OS or
		// power failure can cost the last few commits. FULL would cap
		// throughput at disk fsync rate. A deliberate trade, see D2.
		"_pragma=synchronous(NORMAL)",
		"_pragma=foreign_keys(ON)",
		fmt.Sprintf("_pragma=busy_timeout(%d)", opts.BusyTimeout),
	}
	if writer {
		params = append(params, "_txlock=immediate")
	}
	return "file:" + p + "?" + strings.Join(params, "&")
}

// verifyPragmas confirms the settings applied on both pools.
func (db *DB) verifyPragmas(ctx context.Context) error {
	for _, pool := range []struct {
		name  string
		sqlDB *sql.DB
	}{
		{"write", db.write},
		{"read", db.read},
	} {
		var journal string
		if err := pool.sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			return fmt.Errorf("read journal_mode on %s pool: %w", pool.name, err)
		}
		if !strings.EqualFold(journal, "wal") {
			return fmt.Errorf("journal_mode is %q on the %s pool, want WAL", journal, pool.name)
		}

		var fk int
		if err := pool.sqlDB.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			return fmt.Errorf("read foreign_keys on %s pool: %w", pool.name, err)
		}
		if fk != 1 {
			return fmt.Errorf("foreign_keys is off on the %s pool", pool.name)
		}

		var busy int
		if err := pool.sqlDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			return fmt.Errorf("read busy_timeout on %s pool: %w", pool.name, err)
		}
		if busy <= 0 {
			return fmt.Errorf("busy_timeout is %d on the %s pool, want a positive value", busy, pool.name)
		}
	}
	return nil
}

// Path is the absolute path of the database file.
func (db *DB) Path() string { return db.path }

// Read returns the reader pool. Never use it inside a write transaction; take
// the queries off the *sql.Tx instead, or the transaction's snapshot and the
// read will disagree.
func (db *DB) Read() *sql.DB { return db.read }

// Writer exposes the single-connection write pool for callers that genuinely
// need a raw handle. Prefer Tx.
func (db *DB) Writer() *sql.DB { return db.write }

// Tx runs fn inside a write transaction, committing if it returns nil and
// rolling back otherwise.
//
// This is the only sanctioned write path. Because the write pool holds exactly
// one connection, fn must not call Tx again or issue queries on Writer — it
// would wait for a connection it is itself holding, and deadlock. Reads inside
// fn belong on the *sql.Tx.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write transaction: %w", err)
	}
	defer func() {
		// No-op once the transaction has been committed.
		_ = tx.Rollback()
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write transaction: %w", err)
	}
	return nil
}

// Close shuts both pools down. It attempts a WAL checkpoint first so the
// sidecar files do not outlive a clean shutdown.
func (db *DB) Close() error {
	var errs []error
	if db.write != nil {
		if _, err := db.write.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			errs = append(errs, fmt.Errorf("checkpoint wal: %w", err))
		}
	}
	if db.read != nil {
		if err := db.read.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close read pool: %w", err))
		}
	}
	if db.write != nil {
		if err := db.write.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close write pool: %w", err))
		}
	}
	return errors.Join(errs...)
}
