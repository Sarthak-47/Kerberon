package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// ErrNoOpenIncident reports that a group key has no incident in an open state.
var ErrNoOpenIncident = errors.New("no open incident for group key")

const incidentColumns = `id, group_key, team, policy, policy_snapshot, severity, title,
	status, current_step, alert_count, created_at, acknowledged_at, acknowledged_by,
	resolved_at, resolved_by, last_alert_at, dedup_count`

func scanIncident(row interface{ Scan(...any) error }) (core.Incident, error) {
	var (
		inc                    core.Incident
		severity, status       string
		createdAt, lastAlertAt int64
		ackAt, resolvedAt      sql.NullInt64
		ackBy, resolvedBy      sql.NullString
	)
	err := row.Scan(&inc.ID, &inc.GroupKey, &inc.Team, &inc.Policy, &inc.PolicySnapshot,
		&severity, &inc.Title, &status, &inc.CurrentStep, &inc.AlertCount,
		&createdAt, &ackAt, &ackBy, &resolvedAt, &resolvedBy, &lastAlertAt, &inc.DedupCount)
	if err != nil {
		return core.Incident{}, err
	}
	inc.Severity = core.Severity(severity)
	inc.Status = core.IncidentStatus(status)
	inc.CreatedAt = fromUnix(createdAt)
	inc.LastAlertAt = fromUnix(lastAlertAt)
	inc.AcknowledgedAt = fromUnixPtr(ackAt)
	inc.AcknowledgedBy = ackBy.String
	inc.ResolvedAt = fromUnixPtr(resolvedAt)
	inc.ResolvedBy = resolvedBy.String
	return inc, nil
}

// InsertIncident creates an incident and returns its id.
//
// The partial unique index on group_key makes a second open incident for the
// same group impossible, so a concurrent insert fails here rather than
// producing a duplicate page. Callers should treat a constraint error as "the
// other writer won" and re-read.
func InsertIncident(ctx context.Context, q Queryer, inc core.Incident) (int64, error) {
	if !inc.Severity.Valid() {
		return 0, fmt.Errorf("invalid severity %q", inc.Severity)
	}
	if !inc.Status.Valid() {
		return 0, fmt.Errorf("invalid incident status %q", inc.Status)
	}
	if inc.GroupKey == "" {
		return 0, errors.New("incident requires a group key")
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO incidents
			(group_key, team, policy, policy_snapshot, severity, title, status,
			 current_step, alert_count, created_at, last_alert_at, dedup_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inc.GroupKey, inc.Team, inc.Policy, inc.PolicySnapshot, string(inc.Severity),
		inc.Title, string(inc.Status), inc.CurrentStep, inc.AlertCount,
		toUnix(inc.CreatedAt), toUnix(inc.LastAlertAt), inc.DedupCount)
	if err != nil {
		return 0, fmt.Errorf("insert incident: %w", err)
	}
	return res.LastInsertId()
}

// OpenIncidentByGroupKey returns the incident currently occupying a group key.
// At most one can exist, enforced by the partial unique index.
func OpenIncidentByGroupKey(ctx context.Context, q Queryer, groupKey string) (core.Incident, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+incidentColumns+` FROM incidents
		WHERE group_key = ? AND status IN ('triggered','acknowledged')`, groupKey)
	inc, err := scanIncident(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Incident{}, ErrNoOpenIncident
	}
	if err != nil {
		return core.Incident{}, fmt.Errorf("read open incident: %w", err)
	}
	return inc, nil
}

// LoadIncident reads one incident by id on any Queryer, so a timer handler can
// read it inside the transaction that will also modify it.
func LoadIncident(ctx context.Context, q Queryer, id int64) (core.Incident, error) {
	row := q.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = ?`, id)
	inc, err := scanIncident(row)
	if err != nil {
		return core.Incident{}, fmt.Errorf("load incident %d: %w", id, err)
	}
	return inc, nil
}

// Incident reads one incident by id regardless of status.
func (db *DB) Incident(ctx context.Context, id int64) (core.Incident, error) {
	row := db.read.QueryRowContext(ctx, `SELECT `+incidentColumns+` FROM incidents WHERE id = ?`, id)
	inc, err := scanIncident(row)
	if err != nil {
		return core.Incident{}, fmt.Errorf("read incident %d: %w", id, err)
	}
	return inc, nil
}

// Incidents lists incidents newest first, optionally filtered by status.
func (db *DB) Incidents(ctx context.Context, statuses []core.IncidentStatus, limit int) ([]core.Incident, error) {
	query := `SELECT ` + incidentColumns + ` FROM incidents`
	var args []any
	if len(statuses) > 0 {
		query += ` WHERE status IN (`
		for i, s := range statuses {
			if i > 0 {
				query += `,`
			}
			query += `?`
			args = append(args, string(s))
		}
		query += `)`
	}
	query += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	defer rows.Close()

	var out []core.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// TouchIncident records that a new alert joined an open incident.
//
// dedupIncrement is 1 when the arriving alert repeats a fingerprint the
// incident has already seen, and 0 when it is genuinely new. Distinguishing
// them is what makes the dedup ratio in the UI meaningful.
func TouchIncident(ctx context.Context, q Queryer, id int64, at time.Time, alertIncrement, dedupIncrement int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE incidents
		SET last_alert_at = ?, alert_count = alert_count + ?, dedup_count = dedup_count + ?
		WHERE id = ?`,
		toUnix(at), alertIncrement, dedupIncrement, id)
	if err != nil {
		return fmt.Errorf("touch incident %d: %w", id, err)
	}
	return nil
}

// ResolveIncident closes an incident. resolvedBy is a user id, or "auto" when
// closed by a resolve signal rather than a human.
//
// It only affects open incidents, so resolving twice is a no-op rather than
// overwriting who resolved it first.
func ResolveIncident(ctx context.Context, q Queryer, id int64, at time.Time, resolvedBy string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE incidents
		SET status = 'resolved', resolved_at = ?, resolved_by = ?
		WHERE id = ? AND status IN ('triggered','acknowledged')`,
		toUnix(at), resolvedBy, id)
	if err != nil {
		return false, fmt.Errorf("resolve incident %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("resolve incident %d: %w", id, err)
	}
	return n > 0, nil
}

// AcknowledgeIncident marks an incident taken. It only affects triggered
// incidents, so acknowledging an already-acknowledged or resolved incident is a
// no-op rather than an error — an ack landing late is ordinary, not
// exceptional.
func AcknowledgeIncident(ctx context.Context, q Queryer, id int64, at time.Time, userID string) (bool, error) {
	res, err := q.ExecContext(ctx, `
		UPDATE incidents
		SET status = 'acknowledged', acknowledged_at = ?, acknowledged_by = ?
		WHERE id = ? AND status = 'triggered'`,
		toUnix(at), userID, id)
	if err != nil {
		return false, fmt.Errorf("acknowledge incident %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acknowledge incident %d: %w", id, err)
	}
	return n > 0, nil
}

// Resolve closes an incident and cancels everything still pending for it.
//
// Resolving is the one action that ends an incident outright, so it also stops
// escalation, the ack timeout and any resolve grace window in the same
// transaction. It reports whether the incident was open.
//
// The caller supplies the time. store holds no clock, and reaching for
// time.Now here would put the one package that must stay testable-by-injection
// out of reach of a fake clock.
func (db *DB) Resolve(ctx context.Context, id int64, by string, now time.Time) (bool, error) {
	var closed bool
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		closed, err = ResolveIncident(ctx, tx, id, now, by)
		if err != nil || !closed {
			return err
		}
		if _, err := CancelIncidentTimers(ctx, tx, id, now); err != nil {
			return err
		}
		detail := `{"by":"` + by + `","reason":"resolved manually"}`
		return InsertEvent(ctx, tx, id, core.EventResolved, detail, now)
	})
	return closed, err
}

// ─── Events ───────────────────────────────────────────────────────────────

// InsertEvent appends to an incident's timeline.
func InsertEvent(ctx context.Context, q Queryer, incidentID int64, kind core.EventKind, detail string, at time.Time) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO events (incident_id, kind, detail, created_at) VALUES (?, ?, ?, ?)`,
		incidentID, string(kind), detail, toUnix(at))
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// Events returns an incident's timeline, oldest first.
func (db *DB) Events(ctx context.Context, incidentID int64) ([]core.Event, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT id, incident_id, kind, detail, created_at FROM events
		WHERE incident_id = ? ORDER BY created_at ASC, id ASC`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	var out []core.Event
	for rows.Next() {
		var (
			e         core.Event
			incID     sql.NullInt64
			kind      string
			detail    sql.NullString
			createdAt int64
		)
		if err := rows.Scan(&e.ID, &incID, &kind, &detail, &createdAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if incID.Valid {
			id := incID.Int64
			e.IncidentID = &id
		}
		e.Kind = core.EventKind(kind)
		e.Detail = detail.String
		e.CreatedAt = fromUnix(createdAt)
		out = append(out, e)
	}
	return out, rows.Err()
}
