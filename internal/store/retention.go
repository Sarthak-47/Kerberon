package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RetentionResult reports what a prune removed.
type RetentionResult struct {
	Incidents int64
	// Alerts counts orphans only. An alert attached to an incident is removed
	// by cascade when that incident goes.
	Alerts    int64
	Events    int64
	Timers    int64
	Reclaimed int64
	VacuumRan bool
}

// Prune deletes closed incidents older than the cutoff.
//
// Only closed incidents are eligible. An incident still triggered, however old,
// is something nobody has answered, and deleting it would quietly discard the
// most important thing in the database.
//
// Alerts, events, notifications and timers belonging to a pruned incident go
// with it by foreign-key cascade, so this is one DELETE rather than five that
// could disagree with each other.
func (db *DB) Prune(ctx context.Context, before time.Time) (RetentionResult, error) {
	var res RetentionResult

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		out, err := tx.ExecContext(ctx, `
			DELETE FROM incidents
			WHERE status IN ('resolved','expired','suppressed')
			  AND COALESCE(resolved_at, last_alert_at, created_at) < ?`,
			toUnix(before))
		if err != nil {
			return fmt.Errorf("prune incidents: %w", err)
		}
		res.Incidents, _ = out.RowsAffected()

		// Alerts that never joined an incident have no parent to cascade from.
		// They are usually resolutions for groups that were already closed.
		out, err = tx.ExecContext(ctx, `
			DELETE FROM alerts WHERE incident_id IS NULL AND received_at < ?`,
			toUnix(before))
		if err != nil {
			return fmt.Errorf("prune orphan alerts: %w", err)
		}
		res.Alerts, _ = out.RowsAffected()

		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// Vacuum reclaims space from deleted rows and returns how many bytes went
// back to the filesystem.
//
// SQLite does not shrink a file on DELETE; without this a database that has
// pruned a year of incidents still occupies a year of disk. It cannot run
// inside a transaction, and it takes the write lock for its duration, which is
// why it belongs on a nightly timer rather than after every prune.
func (db *DB) Vacuum(ctx context.Context) (int64, error) {
	before, err := db.sizeOnDisk(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := db.write.ExecContext(ctx, `VACUUM`); err != nil {
		return 0, fmt.Errorf("vacuum: %w", err)
	}
	after, err := db.sizeOnDisk(ctx)
	if err != nil {
		return 0, err
	}
	if reclaimed := before - after; reclaimed > 0 {
		return reclaimed, nil
	}
	return 0, nil
}

func (db *DB) sizeOnDisk(ctx context.Context) (int64, error) {
	var pageCount, pageSize int64
	if err := db.read.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("read page_count: %w", err)
	}
	if err := db.read.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("read page_size: %w", err)
	}
	return pageCount * pageSize, nil
}
