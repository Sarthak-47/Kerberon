package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

const notificationColumns = `id, incident_id, idempotency_key, step_index, target_user,
	channel, destination, body, state, attempts, next_attempt_at, last_error,
	created_at, sent_at, title, severity, ack_url`

func scanNotification(row interface{ Scan(...any) error }) (core.Notification, error) {
	var (
		n                 core.Notification
		channel, state    string
		createdAt         int64
		nextAttempt, sent sql.NullInt64
		lastErr           sql.NullString
		severity          string
	)
	err := row.Scan(&n.ID, &n.IncidentID, &n.IdempotencyKey, &n.StepIndex, &n.TargetUser,
		&channel, &n.Destination, &n.Body, &state, &n.Attempts, &nextAttempt, &lastErr,
		&createdAt, &sent, &n.Title, &severity, &n.AckURL)
	if err != nil {
		return core.Notification{}, err
	}
	n.Channel = core.Channel(channel)
	n.State = core.NotificationState(state)
	n.CreatedAt = fromUnix(createdAt)
	n.NextAttemptAt = fromUnixPtr(nextAttempt)
	n.SentAt = fromUnixPtr(sent)
	n.Severity = core.Severity(severity)
	n.LastError = lastErr.String
	return n, nil
}

// IdempotencyKey derives the outbox key for one notification.
//
//	sha256(incident_id | step_index | target_user | channel | attempt_group)
//
// attempt_group distinguishes a deliberate re-page — the second pass of a
// repeating policy — from a retry of the same one. Fields are length-prefixed
// rather than delimited so a user id containing the separator cannot make two
// distinct notifications hash alike.
func IdempotencyKey(incidentID int64, step int, targetUser string, channel core.Channel, attemptGroup int) string {
	h := sha256.New()
	var num [8]byte

	binary.BigEndian.PutUint64(num[:], uint64(incidentID))
	h.Write(num[:])
	binary.BigEndian.PutUint64(num[:], uint64(step))
	h.Write(num[:])

	binary.BigEndian.PutUint64(num[:], uint64(len(targetUser)))
	h.Write(num[:])
	h.Write([]byte(targetUser))

	binary.BigEndian.PutUint64(num[:], uint64(len(channel)))
	h.Write(num[:])
	h.Write([]byte(channel))

	binary.BigEndian.PutUint64(num[:], uint64(attemptGroup))
	h.Write(num[:])

	return hex.EncodeToString(h.Sum(nil))
}

// ErrDuplicateNotification reports that this exact page has already been
// enqueued. It is an expected outcome, not a failure: it is precisely the
// mechanism that turns at-least-once machinery into "exactly once, from the
// human's perspective" (spec section 8.2).
var ErrDuplicateNotification = errors.New("notification already enqueued")

// EnqueueNotification adds a page to the outbox.
//
// It must run in the same transaction that advances the incident, so a crash
// between "state advanced" and "page enqueued" cannot lose the page.
func EnqueueNotification(ctx context.Context, q Queryer, n core.Notification) (int64, error) {
	if !n.Channel.Valid() {
		return 0, fmt.Errorf("invalid channel %q", n.Channel)
	}
	if !n.State.Valid() {
		return 0, fmt.Errorf("invalid notification state %q", n.State)
	}
	if n.IdempotencyKey == "" {
		return 0, errors.New("notification requires an idempotency key")
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO notifications
			(incident_id, idempotency_key, step_index, target_user, channel,
			 destination, body, state, attempts, next_attempt_at, created_at,
			 title, severity, ack_url)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.IncidentID, n.IdempotencyKey, n.StepIndex, n.TargetUser, string(n.Channel),
		n.Destination, n.Body, string(n.State), n.Attempts,
		toUnixPtr(n.NextAttemptAt), toUnix(n.CreatedAt),
		n.Title, string(n.Severity), n.AckURL)
	if err != nil {
		// The UNIQUE constraint is the point: a retried escalation cannot
		// enqueue the same page twice.
		if isUniqueViolation(err) {
			return 0, ErrDuplicateNotification
		}
		return 0, fmt.Errorf("enqueue notification: %w", err)
	}
	return res.LastInsertId()
}

func isUniqueViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// ClaimDueNotifications marks up to limit due notifications as sending and
// returns them.
//
// Claiming and reading happen in one statement so two workers cannot take the
// same row. A row stuck in sending — the worker died mid-send — is reclaimed
// once its lease expires; see DECISIONS D7 for why the ambiguity resolves
// toward retrying.
func (db *DB) ClaimDueNotifications(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]core.Notification, error) {
	if limit <= 0 {
		limit = 1
	}

	var claimed []core.Notification
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+notificationColumns+` FROM notifications
			WHERE (state IN ('pending','failed')
			       AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
			   OR (state = 'sending' AND created_at <= ?)
			ORDER BY next_attempt_at ASC, id ASC
			LIMIT ?`,
			toUnix(now), toUnix(now.Add(-lease)), limit)
		if err != nil {
			return fmt.Errorf("select due notifications: %w", err)
		}

		var found []core.Notification
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				rows.Close()
				return fmt.Errorf("scan notification: %w", err)
			}
			found = append(found, n)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for i := range found {
			res, err := tx.ExecContext(ctx, `
				UPDATE notifications SET state = 'sending', attempts = attempts + 1
				WHERE id = ? AND state IN ('pending','failed','sending')`, found[i].ID)
			if err != nil {
				return fmt.Errorf("claim notification %d: %w", found[i].ID, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // someone else took it
			}
			found[i].State = core.NotifSending
			found[i].Attempts++
			claimed = append(claimed, found[i])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkNotificationSent records a successful delivery.
func MarkNotificationSent(ctx context.Context, q Queryer, id int64, at time.Time) error {
	_, err := q.ExecContext(ctx, `
		UPDATE notifications SET state = 'sent', sent_at = ?, last_error = NULL
		WHERE id = ?`, toUnix(at), id)
	if err != nil {
		return fmt.Errorf("mark notification %d sent: %w", id, err)
	}
	return nil
}

// MarkNotificationFailed schedules a retry.
func MarkNotificationFailed(ctx context.Context, q Queryer, id int64, retryAt time.Time, cause string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE notifications SET state = 'failed', next_attempt_at = ?, last_error = ?
		WHERE id = ?`, toUnix(retryAt), truncateError(cause), id)
	if err != nil {
		return fmt.Errorf("mark notification %d failed: %w", id, err)
	}
	return nil
}

// MarkNotificationDead records that every retry was exhausted.
//
// A dead notification is not the end of it: failing to page is itself a
// critical condition, and the dispatcher raises an internal incident for it
// rather than letting the page vanish (spec section 8.3).
func MarkNotificationDead(ctx context.Context, q Queryer, id int64, cause string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE notifications SET state = 'dead', next_attempt_at = NULL, last_error = ?
		WHERE id = ?`, truncateError(cause), id)
	if err != nil {
		return fmt.Errorf("mark notification %d dead: %w", id, err)
	}
	return nil
}

// truncateError bounds what a misbehaving provider can write into the database.
func truncateError(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// Notification reads one row.
func (db *DB) Notification(ctx context.Context, id int64) (core.Notification, error) {
	row := db.read.QueryRowContext(ctx,
		`SELECT `+notificationColumns+` FROM notifications WHERE id = ?`, id)
	n, err := scanNotification(row)
	if err != nil {
		return core.Notification{}, fmt.Errorf("read notification %d: %w", id, err)
	}
	return n, nil
}

// NotificationsForIncident lists an incident's outbox rows, oldest first, for
// the timeline view.
func (db *DB) NotificationsForIncident(ctx context.Context, incidentID int64) ([]core.Notification, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+notificationColumns+` FROM notifications
		WHERE incident_id = ? ORDER BY created_at ASC, id ASC`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("read notifications for incident %d: %w", incidentID, err)
	}
	defer rows.Close()

	var out []core.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountNotificationsByState is used by the UI and by tests asserting how many
// pages a cascade actually produced.
func (db *DB) CountNotificationsByState(ctx context.Context) (map[core.NotificationState]int, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM notifications GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("count notifications: %w", err)
	}
	defer rows.Close()

	out := map[core.NotificationState]int{}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[core.NotificationState(state)] = n
	}
	return out, rows.Err()
}
