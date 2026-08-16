package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// Queryer is satisfied by both *sql.DB and *sql.Tx, so timer helpers can run
// either standalone or inside a caller's transaction. Timer effects must join
// the transaction that marks the timer complete (DECISIONS D1), which is why
// these take a Queryer rather than a *DB.
type Queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrTimerNotPending is returned when a timer has already completed, been
// cancelled, or no longer exists. It is an expected outcome, not a failure: an
// acknowledgement can cancel a timer between the scheduler selecting it and the
// scheduler executing it.
var ErrTimerNotPending = errors.New("timer is not pending")

// ─── Time conversion ──────────────────────────────────────────────────────
//
// The database stores Unix epoch seconds in UTC. Conversion lives here and
// nowhere else.

func toUnix(t time.Time) int64 { return t.UTC().Unix() }

func toUnixPtr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: toUnix(*t), Valid: true}
}

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

func fromUnixPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromUnix(v.Int64)
	return &t
}

// ─── Timers ───────────────────────────────────────────────────────────────

const timerColumns = `id, incident_id, kind, fire_at, payload, created_at,
	claimed_at, completed_at, cancelled_at`

// InsertTimer schedules a future action. Callers inside an escalation effect
// must pass the effect's transaction so the new timer commits atomically with
// the state change that caused it.
func InsertTimer(ctx context.Context, q Queryer, t core.Timer) (int64, error) {
	if !t.Kind.Valid() {
		return 0, fmt.Errorf("invalid timer kind %q", t.Kind)
	}
	if t.IncidentID == 0 {
		return 0, errors.New("timer requires an incident id")
	}
	if t.CreatedAt.IsZero() {
		return 0, errors.New("timer requires a created_at")
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO timers (incident_id, kind, fire_at, payload, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		t.IncidentID, string(t.Kind), toUnix(t.FireAt), t.Payload, toUnix(t.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("insert timer: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("insert timer: %w", err)
	}
	return id, nil
}

func scanTimer(row interface{ Scan(...any) error }) (core.Timer, error) {
	var (
		t                             core.Timer
		kind                          string
		fireAt, createdAt             int64
		payload                       sql.NullString
		claimed, completed, cancelled sql.NullInt64
	)
	err := row.Scan(&t.ID, &t.IncidentID, &kind, &fireAt, &payload, &createdAt,
		&claimed, &completed, &cancelled)
	if err != nil {
		return core.Timer{}, err
	}
	t.Kind = core.TimerKind(kind)
	t.FireAt = fromUnix(fireAt)
	t.CreatedAt = fromUnix(createdAt)
	t.Payload = payload.String
	t.ClaimedAt = fromUnixPtr(claimed)
	t.CompletedAt = fromUnixPtr(completed)
	t.CancelledAt = fromUnixPtr(cancelled)
	return t, nil
}

// LoadPendingTimer reads a timer and confirms it is still eligible to fire.
//
// The scheduler calls this inside the transaction that will execute the effect,
// which is what makes the pending check and the effect atomic. A cancellation
// that commits first makes this return ErrTimerNotPending; one that commits
// after is serialized behind the single writer and sees a completed timer.
func LoadPendingTimer(ctx context.Context, q Queryer, id int64) (core.Timer, error) {
	row := q.QueryRowContext(ctx, `SELECT `+timerColumns+` FROM timers WHERE id = ?`, id)
	t, err := scanTimer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Timer{}, ErrTimerNotPending
	}
	if err != nil {
		return core.Timer{}, fmt.Errorf("load timer %d: %w", id, err)
	}
	if !t.Pending() {
		return core.Timer{}, ErrTimerNotPending
	}
	return t, nil
}

// CompleteTimer marks a timer executed. It must run in the same transaction as
// the effect (D1).
func CompleteTimer(ctx context.Context, q Queryer, id int64, at time.Time) error {
	res, err := q.ExecContext(ctx, `
		UPDATE timers SET completed_at = ?
		WHERE id = ? AND completed_at IS NULL AND cancelled_at IS NULL`,
		toUnix(at), id)
	if err != nil {
		return fmt.Errorf("complete timer %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete timer %d: %w", id, err)
	}
	if n == 0 {
		return ErrTimerNotPending
	}
	return nil
}

// CancelTimer stops a pending timer from ever executing. Cancelling an already
// finished timer is a no-op rather than an error: an acknowledgement arriving
// just as a step fires is normal, not exceptional.
func CancelTimer(ctx context.Context, q Queryer, id int64, at time.Time) error {
	_, err := q.ExecContext(ctx, `
		UPDATE timers SET cancelled_at = ?
		WHERE id = ? AND completed_at IS NULL AND cancelled_at IS NULL`,
		toUnix(at), id)
	if err != nil {
		return fmt.Errorf("cancel timer %d: %w", id, err)
	}
	return nil
}

// CancelIncidentTimers cancels every pending timer for an incident, optionally
// restricted to certain kinds. This is what an acknowledgement or a resolution
// does to pending escalation timers. It returns how many were cancelled.
func CancelIncidentTimers(ctx context.Context, q Queryer, incidentID int64, at time.Time, kinds ...core.TimerKind) (int64, error) {
	query := `UPDATE timers SET cancelled_at = ?
		WHERE incident_id = ? AND completed_at IS NULL AND cancelled_at IS NULL`
	args := []any{toUnix(at), incidentID}

	if len(kinds) > 0 {
		query += ` AND kind IN (`
		for i, k := range kinds {
			if i > 0 {
				query += `,`
			}
			query += `?`
			args = append(args, string(k))
		}
		query += `)`
	}

	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("cancel timers for incident %d: %w", incidentID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel timers for incident %d: %w", incidentID, err)
	}
	return n, nil
}

// EarliestPendingTimer returns the next timer due to fire.
//
// On startup this naturally returns timers whose fire_at is already in the
// past, in fire_at order, so crash recovery needs no separate code path: a
// process that was down for ten minutes simply finds overdue timers and works
// through them oldest first.
func (db *DB) EarliestPendingTimer(ctx context.Context) (core.Timer, bool, error) {
	row := db.read.QueryRowContext(ctx, `
		SELECT `+timerColumns+` FROM timers
		WHERE completed_at IS NULL AND cancelled_at IS NULL
		ORDER BY fire_at ASC, id ASC
		LIMIT 1`)
	t, err := scanTimer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Timer{}, false, nil
	}
	if err != nil {
		return core.Timer{}, false, fmt.Errorf("earliest pending timer: %w", err)
	}
	return t, true, nil
}

// NextPendingTimers returns up to limit timers due soonest, earliest first.
//
// The scheduler takes a small batch rather than a single row so that a timer
// whose handler is currently backing off cannot block the ones behind it.
func (db *DB) NextPendingTimers(ctx context.Context, limit int) ([]core.Timer, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+timerColumns+` FROM timers
		WHERE completed_at IS NULL AND cancelled_at IS NULL
		ORDER BY fire_at ASC, id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("next pending timers: %w", err)
	}
	defer rows.Close()

	var out []core.Timer
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending timer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PendingTimers lists every timer still eligible to fire, earliest first.
func (db *DB) PendingTimers(ctx context.Context) ([]core.Timer, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+timerColumns+` FROM timers
		WHERE completed_at IS NULL AND cancelled_at IS NULL
		ORDER BY fire_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list pending timers: %w", err)
	}
	defer rows.Close()

	var out []core.Timer
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending timer: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Timer reads one timer regardless of state, for tests and the incident
// timeline view.
func (db *DB) Timer(ctx context.Context, id int64) (core.Timer, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+timerColumns+` FROM timers WHERE id = ?`, id)
	t, err := scanTimer(row)
	if err != nil {
		return core.Timer{}, fmt.Errorf("read timer %d: %w", id, err)
	}
	return t, nil
}
