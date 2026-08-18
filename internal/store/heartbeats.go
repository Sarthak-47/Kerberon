package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

const heartbeatColumns = `id, name, token, expected_interval, grace_period, team,
	severity, last_ping_at, state, created_at`

func scanHeartbeat(row interface{ Scan(...any) error }) (core.Heartbeat, error) {
	var (
		h               core.Heartbeat
		severity, state string
		expected, grace int64
		createdAt       int64
		lastPing        sql.NullInt64
	)
	err := row.Scan(&h.ID, &h.Name, &h.Token, &expected, &grace, &h.Team,
		&severity, &lastPing, &state, &createdAt)
	if err != nil {
		return core.Heartbeat{}, err
	}
	h.ExpectedInterval = time.Duration(expected) * time.Second
	h.GracePeriod = time.Duration(grace) * time.Second
	h.Severity = core.Severity(severity)
	h.State = core.HeartbeatState(state)
	h.LastPingAt = fromUnixPtr(lastPing)
	h.CreatedAt = fromUnix(createdAt)
	return h, nil
}

// NewHeartbeatToken mints the secret that identifies a heartbeat.
//
// It goes in a URL that a cron job holds, so it must be unguessable: anyone
// who can guess it can keep a dead job looking alive, which is precisely the
// failure a dead-man's switch exists to catch.
func NewHeartbeatToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate heartbeat token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// InsertHeartbeat registers a dead-man's switch.
func InsertHeartbeat(ctx context.Context, q Queryer, h core.Heartbeat) (int64, error) {
	if h.Name == "" {
		return 0, errors.New("heartbeat requires a name")
	}
	if h.ExpectedInterval <= 0 {
		return 0, errors.New("heartbeat requires a positive expected_interval")
	}
	if !h.Severity.Valid() {
		h.Severity = core.SeverityCritical
	}
	if h.State == "" {
		// Never pinged is a distinct state from missing: a switch that has
		// never reported may simply not be deployed yet.
		h.State = core.HeartbeatNeverSeen
	}

	res, err := q.ExecContext(ctx, `
		INSERT INTO heartbeats
			(name, token, expected_interval, grace_period, team, severity,
			 last_ping_at, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.Name, h.Token, int64(h.ExpectedInterval.Seconds()), int64(h.GracePeriod.Seconds()),
		h.Team, string(h.Severity), toUnixPtr(h.LastPingAt), string(h.State), toUnix(h.CreatedAt))
	if err != nil {
		return 0, fmt.Errorf("insert heartbeat: %w", err)
	}
	return res.LastInsertId()
}

// ErrUnknownHeartbeat reports a token that matches nothing.
var ErrUnknownHeartbeat = errors.New("unknown heartbeat token")

// RecordPing marks a heartbeat alive. It returns the heartbeat as it was
// before the ping, so a caller can tell whether this ping is a recovery.
func (db *DB) RecordPing(ctx context.Context, token string, at time.Time) (core.Heartbeat, error) {
	var before core.Heartbeat

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+heartbeatColumns+` FROM heartbeats WHERE token = ?`, token)
		h, err := scanHeartbeat(row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknownHeartbeat
		}
		if err != nil {
			return fmt.Errorf("read heartbeat: %w", err)
		}
		before = h

		_, err = tx.ExecContext(ctx,
			`UPDATE heartbeats SET last_ping_at = ?, state = 'healthy' WHERE id = ?`,
			toUnix(at), h.ID)
		return err
	})
	return before, err
}

// OverdueHeartbeats returns the switches that have missed their window.
//
// A heartbeat that has never been pinged is excluded: it is not late, it has
// not started. Reporting it as missing would page somebody about a job that
// was never deployed.
func (db *DB) OverdueHeartbeats(ctx context.Context, now time.Time) ([]core.Heartbeat, error) {
	rows, err := db.read.QueryContext(ctx, `
		SELECT `+heartbeatColumns+` FROM heartbeats
		WHERE state = 'healthy'
		  AND last_ping_at IS NOT NULL
		  AND last_ping_at + expected_interval + grace_period < ?`,
		toUnix(now))
	if err != nil {
		return nil, fmt.Errorf("read overdue heartbeats: %w", err)
	}
	defer rows.Close()

	var out []core.Heartbeat
	for rows.Next() {
		h, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MarkHeartbeatMissing flips a switch to missing. It reports whether the state
// actually changed, so the sweeper raises one incident rather than one per
// sweep.
func MarkHeartbeatMissing(ctx context.Context, q Queryer, id int64) (bool, error) {
	res, err := q.ExecContext(ctx,
		`UPDATE heartbeats SET state = 'missing' WHERE id = ? AND state = 'healthy'`, id)
	if err != nil {
		return false, fmt.Errorf("mark heartbeat %d missing: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Heartbeats lists every registered switch.
func (db *DB) Heartbeats(ctx context.Context) ([]core.Heartbeat, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT `+heartbeatColumns+` FROM heartbeats ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list heartbeats: %w", err)
	}
	defer rows.Close()

	var out []core.Heartbeat
	for rows.Next() {
		h, err := scanHeartbeat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
