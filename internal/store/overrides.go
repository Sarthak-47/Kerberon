package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

const overrideColumns = `id, schedule_name, user_id, starts_at, ends_at, reason,
	created_at, created_by`

func scanOverride(row interface{ Scan(...any) error }) (core.Override, error) {
	var (
		o                           core.Override
		startsAt, endsAt, createdAt int64
		reason                      sql.NullString
	)
	err := row.Scan(&o.ID, &o.ScheduleName, &o.UserID, &startsAt, &endsAt,
		&reason, &createdAt, &o.CreatedBy)
	if err != nil {
		return core.Override{}, err
	}
	o.StartsAt = fromUnix(startsAt)
	o.EndsAt = fromUnix(endsAt)
	o.CreatedAt = fromUnix(createdAt)
	o.Reason = reason.String
	return o, nil
}

// InsertOverride records a cover. Overrides are state rather than config
// because forcing a git commit for a last-minute swap would be user-hostile
// (spec section 4.2).
func InsertOverride(ctx context.Context, q Queryer, o core.Override) (int64, error) {
	if o.ScheduleName == "" {
		return 0, errors.New("override requires a schedule name")
	}
	if o.UserID == "" {
		return 0, errors.New("override requires a user")
	}
	if !o.EndsAt.After(o.StartsAt) {
		return 0, fmt.Errorf("override ends at %s, which is not after its start %s",
			o.EndsAt.Format(time.RFC3339), o.StartsAt.Format(time.RFC3339))
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO overrides
			(schedule_name, user_id, starts_at, ends_at, reason, created_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		o.ScheduleName, o.UserID, toUnix(o.StartsAt), toUnix(o.EndsAt),
		o.Reason, toUnix(o.CreatedAt), o.CreatedBy)
	if err != nil {
		return 0, fmt.Errorf("insert override: %w", err)
	}
	return res.LastInsertId()
}

// OverridesInWindow returns every override for a schedule that overlaps
// [from, to). Passing a window rather than loading them all keeps a long-lived
// instance's calendar render bounded.
func (db *DB) OverridesInWindow(ctx context.Context, scheduleName string, from, to time.Time) ([]core.Override, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+overrideColumns+` FROM overrides
		WHERE schedule_name = ? AND starts_at < ? AND ends_at > ?
		ORDER BY created_at ASC, id ASC`,
		scheduleName, toUnix(to), toUnix(from))
	if err != nil {
		return nil, fmt.Errorf("read overrides: %w", err)
	}
	defer rows.Close()

	var out []core.Override
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, fmt.Errorf("scan override: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OverridesAt returns the overrides covering a single instant.
func (db *DB) OverridesAt(ctx context.Context, scheduleName string, at time.Time) ([]core.Override, error) {
	// A one-second window either side is not enough: use the half-open
	// containment the resolver applies.
	return db.OverridesInWindow(ctx, scheduleName, at, at.Add(time.Nanosecond))
}

// DeleteOverride removes a cover. Returns whether a row was removed, so a
// caller can distinguish "gone" from "never existed".
func DeleteOverride(ctx context.Context, q Queryer, id int64) (bool, error) {
	res, err := q.ExecContext(ctx, `DELETE FROM overrides WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete override %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete override %d: %w", id, err)
	}
	return n > 0, nil
}
