package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Sarthak-47/kerberon/internal/core"
)

const alertColumns = `id, fingerprint, source, status, labels, annotations,
	starts_at, ends_at, received_at, incident_id, raw`

func scanAlert(row interface{ Scan(...any) error }) (core.Alert, error) {
	var (
		a                    core.Alert
		source, status       string
		labelsJSON, annoJSON string
		startsAt, receivedAt int64
		endsAt, incidentID   sql.NullInt64
		raw                  sql.NullString
	)
	err := row.Scan(&a.ID, &a.Fingerprint, &source, &status, &labelsJSON, &annoJSON,
		&startsAt, &endsAt, &receivedAt, &incidentID, &raw)
	if err != nil {
		return core.Alert{}, err
	}
	a.Source = core.Source(source)
	a.Status = core.AlertStatus(status)
	if err := json.Unmarshal([]byte(labelsJSON), &a.Labels); err != nil {
		return core.Alert{}, fmt.Errorf("decode labels for alert %d: %w", a.ID, err)
	}
	if err := json.Unmarshal([]byte(annoJSON), &a.Annotations); err != nil {
		return core.Alert{}, fmt.Errorf("decode annotations for alert %d: %w", a.ID, err)
	}
	a.StartsAt = fromUnix(startsAt)
	a.ReceivedAt = fromUnix(receivedAt)
	a.EndsAt = fromUnixPtr(endsAt)
	if incidentID.Valid {
		id := incidentID.Int64
		a.IncidentID = &id
	}
	a.Raw = raw.String
	return a, nil
}

// InsertAlert stores a normalized alert.
func InsertAlert(ctx context.Context, q Queryer, a core.Alert) (int64, error) {
	if !a.Source.Valid() {
		return 0, fmt.Errorf("invalid alert source %q", a.Source)
	}
	if !a.Status.Valid() {
		return 0, fmt.Errorf("invalid alert status %q", a.Status)
	}
	if a.Fingerprint == "" {
		return 0, fmt.Errorf("alert requires a fingerprint")
	}

	// Labels and annotations are stored as JSON objects, never null, so
	// readers can decode unconditionally.
	labels := a.Labels
	if labels == nil {
		labels = core.Labels{}
	}
	annotations := a.Annotations
	if annotations == nil {
		annotations = core.Annotations{}
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return 0, fmt.Errorf("encode labels: %w", err)
	}
	annoJSON, err := json.Marshal(annotations)
	if err != nil {
		return 0, fmt.Errorf("encode annotations: %w", err)
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO alerts
			(fingerprint, source, status, labels, annotations, starts_at, ends_at,
			 received_at, incident_id, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Fingerprint, string(a.Source), string(a.Status), string(labelsJSON), string(annoJSON),
		toUnix(a.StartsAt), toUnixPtr(a.EndsAt), toUnix(a.ReceivedAt),
		nullableInt64(a.IncidentID), a.Raw)
	if err != nil {
		return 0, fmt.Errorf("insert alert: %w", err)
	}
	return res.LastInsertId()
}

func nullableInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// AlertsForIncident returns an incident's constituent alerts, newest first.
func (db *DB) AlertsForIncident(ctx context.Context, incidentID int64) ([]core.Alert, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+alertColumns+` FROM alerts
		WHERE incident_id = ? ORDER BY received_at DESC, id DESC`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("read alerts for incident %d: %w", incidentID, err)
	}
	defer rows.Close()

	var out []core.Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// HasFingerprint reports whether an incident already contains an alert with
// this fingerprint. It is how a repeat is distinguished from a genuinely new
// alert joining the group, which is what makes the dedup count meaningful.
func HasFingerprint(ctx context.Context, q Queryer, incidentID int64, fingerprint string) (bool, error) {
	// EXISTS stops at the first match. COUNT(*) with a LIMIT does not: the
	// limit applies to the aggregate's single output row, not to the scan, so
	// it counted every matching alert while looking like a short-circuit.
	var found int
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM alerts WHERE incident_id = ? AND fingerprint = ?)`,
		incidentID, fingerprint).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check fingerprint: %w", err)
	}
	return found == 1, nil
}

// AllAlertsResolved reports whether every distinct fingerprint in an incident
// has a resolved alert as its most recent state.
//
// Comparing latest-per-fingerprint rather than counting rows is what makes
// flapping work: an alert that resolves and re-fires leaves both rows in place,
// and only the newer one should count.
func AllAlertsResolved(ctx context.Context, q Queryer, incidentID int64) (bool, error) {
	// For each fingerprint, take the most recently received alert and check
	// whether any of those is still firing.
	var firing int
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT a.status
			FROM alerts a
			JOIN (
				SELECT fingerprint, MAX(received_at) AS latest, MAX(id) AS latest_id
				FROM alerts WHERE incident_id = ?
				GROUP BY fingerprint
			) newest
			  ON a.fingerprint = newest.fingerprint AND a.id = newest.latest_id
			WHERE a.incident_id = ? AND a.status = 'firing'
		)`, incidentID, incidentID).Scan(&firing)
	if err != nil {
		return false, fmt.Errorf("check resolution for incident %d: %w", incidentID, err)
	}

	// An incident with no alerts at all is not "all resolved"; that would let
	// an empty group close itself.
	var total int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alerts WHERE incident_id = ?`, incidentID).Scan(&total); err != nil {
		return false, fmt.Errorf("count alerts for incident %d: %w", incidentID, err)
	}
	if total == 0 {
		return false, nil
	}
	return firing == 0, nil
}
