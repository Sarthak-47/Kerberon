package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sarthak-47/kerberon/internal/store"
)

// tempDir allocates a scratch directory inside the project's .tmp rather than
// using t.TempDir(), which would write to the system temp directory and breach
// CLAUDE.md R1.
func tempDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "store-test-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// openDB opens a migrated database in a fresh directory.
func openDB(t *testing.T) *store.DB {
	t.Helper()
	db := openUnmigrated(t)
	if _, err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func openUnmigrated(t *testing.T) *store.DB {
	t.Helper()
	path := filepath.Join(tempDir(t), "kerberon.db")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ─── Open and pragmas ─────────────────────────────────────────────────────

func TestOpenCreatesTheFile(t *testing.T) {
	db := openUnmigrated(t)
	if _, err := os.Stat(db.Path()); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
}

// The project path contains spaces, so this exercises URI escaping on every
// run. A DSN that fails to escape produces a database in the wrong place, or a
// confusing "unable to open database file".
func TestOpenHandlesPathsWithSpacesAndSubdirectories(t *testing.T) {
	dir := filepath.Join(tempDir(t), "nested dir with spaces")
	path := filepath.Join(dir, "kerberon.db")

	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open with spaces in path: %v", err)
	}
	defer db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the requested path %q: %v", path, err)
	}
}

// Open asserts its own pragmas, so this checks the assertion is real rather
// than vacuous, and that both pools carry the settings.
func TestPragmasApplyToBothPools(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	pools := map[string]*sql.DB{"read": db.Read(), "write": db.Writer()}
	for name, pool := range pools {
		var journal string
		if err := pool.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("%s pool journal_mode: %v", name, err)
		}
		if !strings.EqualFold(journal, "wal") {
			t.Errorf("%s pool journal_mode = %q, want wal", name, journal)
		}

		var fk int
		if err := pool.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("%s pool foreign_keys: %v", name, err)
		}
		if fk != 1 {
			t.Errorf("%s pool foreign_keys = %d, want 1", name, fk)
		}

		var busy int
		if err := pool.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("%s pool busy_timeout: %v", name, err)
		}
		if busy != 5000 {
			t.Errorf("%s pool busy_timeout = %d, want 5000", name, busy)
		}

		var sync int
		if err := pool.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("%s pool synchronous: %v", name, err)
		}
		if sync != 1 { // 1 == NORMAL
			t.Errorf("%s pool synchronous = %d, want 1 (NORMAL)", name, sync)
		}
	}
}

func TestWritePoolIsSingleConnection(t *testing.T) {
	db := openDB(t)
	if got := db.Writer().Stats().MaxOpenConnections; got != 1 {
		t.Errorf("write pool MaxOpenConnections = %d, want 1", got)
	}
	if got := db.Read().Stats().MaxOpenConnections; got < 2 {
		t.Errorf("read pool MaxOpenConnections = %d, want at least 2", got)
	}
}

// ─── Migrations ───────────────────────────────────────────────────────────

func TestMigrateCreatesEverySchemaObject(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	wantTables := []string{
		"acks", "alerts", "events", "heartbeats", "incidents",
		"notifications", "overrides", "schema_migrations", "timers",
	}
	for _, table := range wantTables {
		var n int
		err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("table %q missing", table)
		}
	}

	wantIndexes := []string{
		"idx_incidents_open_group", "idx_incidents_status",
		"idx_alerts_fingerprint", "idx_alerts_incident",
		"idx_timers_pending", "idx_notifications_due",
		"idx_notifications_sending", "idx_acks_incident",
		"idx_overrides_window", "idx_heartbeats_state", "idx_events_incident",
	}
	for _, idx := range wantIndexes {
		var n int
		err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n)
		if err != nil {
			t.Fatalf("query sqlite_master for %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("index %q missing", idx)
		}
	}
}

// Migrate runs on every startup, so a second run must change nothing.
func TestMigrateIsIdempotent(t *testing.T) {
	db := openUnmigrated(t)
	ctx := context.Background()

	first, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first migrate applied nothing")
	}

	second, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second migrate applied %d migrations, want 0", len(second))
	}
}

func TestSchemaVersion(t *testing.T) {
	db := openUnmigrated(t)
	ctx := context.Background()

	if v, err := db.SchemaVersion(ctx); err != nil || v != 0 {
		t.Fatalf("version before migrating = %d, %v; want 0, nil", v, err)
	}

	applied, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if want := applied[len(applied)-1].Version; got != want {
		t.Errorf("SchemaVersion = %d, want %d", got, want)
	}
}

func TestLoadMigrationsAreOrderedAndWellFormed(t *testing.T) {
	ms, err := store.LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range ms {
		if m.Version <= 0 {
			t.Errorf("migration %d has non-positive version %d", i, m.Version)
		}
		if m.Name == "" {
			t.Errorf("migration %d has an empty name", i)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %04d_%s is empty", m.Version, m.Name)
		}
		if i > 0 && ms[i-1].Version >= m.Version {
			t.Errorf("migrations out of order: %d then %d", ms[i-1].Version, m.Version)
		}
	}
}

// ─── Schema invariants ────────────────────────────────────────────────────

func insertIncident(t *testing.T, db *store.DB, groupKey, status string) (int64, error) {
	t.Helper()
	var id int64
	err := db.Tx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO incidents
				(group_key, team, policy, severity, title, status, created_at, last_alert_at)
			VALUES (?, 'platform', 'critical-24x7', 'critical', 'test', ?, 1000, 1000)`,
			groupKey, status)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// The partial unique index is what makes "one open incident per group" a
// database invariant rather than application-layer hope (spec section 5).
func TestOnlyOneOpenIncidentPerGroupKey(t *testing.T) {
	db := openDB(t)

	if _, err := insertIncident(t, db, "group-a", "triggered"); err != nil {
		t.Fatalf("first triggered incident: %v", err)
	}

	if _, err := insertIncident(t, db, "group-a", "triggered"); err == nil {
		t.Fatal("a second triggered incident for the same group key was allowed")
	}
	// acknowledged is also an open state and must collide.
	if _, err := insertIncident(t, db, "group-a", "acknowledged"); err == nil {
		t.Fatal("an acknowledged incident for the same open group key was allowed")
	}
	// A different group is unaffected.
	if _, err := insertIncident(t, db, "group-b", "triggered"); err != nil {
		t.Fatalf("different group key rejected: %v", err)
	}
}

// Once an incident closes it releases its group key, so the next occurrence can
// open a fresh incident.
func TestClosedIncidentReleasesItsGroupKey(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	id, err := insertIncident(t, db, "group-a", "triggered")
	if err != nil {
		t.Fatalf("first incident: %v", err)
	}

	err = db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE incidents SET status='resolved', resolved_at=2000 WHERE id=?`, id)
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if _, err := insertIncident(t, db, "group-a", "triggered"); err != nil {
		t.Fatalf("new incident after resolve was rejected: %v", err)
	}

	// Both rows persist; only one is open.
	var open int
	err = db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE group_key='group-a' AND status IN ('triggered','acknowledged')`).Scan(&open)
	if err != nil {
		t.Fatalf("count open: %v", err)
	}
	if open != 1 {
		t.Errorf("open incidents for group-a = %d, want 1", open)
	}
}

func TestForeignKeysAreEnforced(t *testing.T) {
	db := openDB(t)
	err := db.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			INSERT INTO timers (incident_id, kind, fire_at, created_at)
			VALUES (99999, 'escalate', 1000, 1000)`)
		return err
	})
	if err == nil {
		t.Fatal("a timer referencing a nonexistent incident was accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Errorf("error = %v, want a foreign key violation", err)
	}
}

// Deleting an incident during retention pruning must not strand its timers or
// outbox rows.
func TestDeletingAnIncidentCascades(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	id, err := insertIncident(t, db, "group-a", "triggered")
	if err != nil {
		t.Fatalf("insert incident: %v", err)
	}

	err = db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO timers (incident_id, kind, fire_at, created_at)
			VALUES (?, 'escalate', 1000, 1000)`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`
			INSERT INTO notifications
				(incident_id, idempotency_key, step_index, target_user, channel,
				 destination, body, state, created_at)
			VALUES (?, 'key-1', 0, 'sarthak', 'ntfy', 'https://ntfy.sh/x', 'body', 'pending', 1000)`, id)
		return err
	})
	if err != nil {
		t.Fatalf("insert children: %v", err)
	}

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM incidents WHERE id=?`, id)
		return err
	}); err != nil {
		t.Fatalf("delete incident: %v", err)
	}

	for _, table := range []string{"timers", "notifications"} {
		var n int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM `+table+` WHERE incident_id=?`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%d orphaned rows left in %s", n, table)
		}
	}
}

// The idempotency key is what makes "exactly once, from the human's
// perspective" achievable on at-least-once machinery (spec section 8.2).
func TestIdempotencyKeyIsUnique(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	id, err := insertIncident(t, db, "group-a", "triggered")
	if err != nil {
		t.Fatalf("insert incident: %v", err)
	}

	insert := func() error {
		return db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				INSERT INTO notifications
					(incident_id, idempotency_key, step_index, target_user, channel,
					 destination, body, state, created_at)
				VALUES (?, 'same-key', 0, 'sarthak', 'ntfy', 'dest', 'body', 'pending', 1000)`, id)
			return err
		})
	}

	if err := insert(); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := insert(); err == nil {
		t.Fatal("a duplicate idempotency key was accepted")
	}
}

// ─── Transactions ─────────────────────────────────────────────────────────

func TestTxRollsBackOnError(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	sentinel := errors.New("deliberate failure")

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			INSERT INTO incidents
				(group_key, team, policy, severity, title, status, created_at, last_alert_at)
			VALUES ('rollback-me', 'platform', 'p', 'critical', 't', 'triggered', 1, 1)`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx returned %v, want the sentinel error", err)
	}

	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE group_key='rollback-me'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows survived a rolled-back transaction", n)
	}
}

func TestTxCommitsOnSuccess(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	if _, err := insertIncident(t, db, "keep-me", "triggered"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var n int
	if err := db.Read().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE group_key='keep-me'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("committed rows = %d, want 1", n)
	}
}

// Readers must not block behind the writer; that is the whole reason for WAL
// and for the split pools.
func TestReadersSeeCommittedWritesImmediately(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if _, err := insertIncident(t, db, key, "triggered"); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
		var n int
		if err := db.Read().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM incidents WHERE group_key=?`, key).Scan(&n); err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if n != 1 {
			t.Errorf("reader did not see committed write for %q", key)
		}
	}
}

func TestCloseIsSafe(t *testing.T) {
	path := filepath.Join(tempDir(t), "kerberon.db")
	db, err := store.Open(context.Background(), path, store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
