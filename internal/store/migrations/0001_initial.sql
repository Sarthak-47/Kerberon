-- Kerberon initial schema. Spec section 5.
--
-- Timestamps are Unix epoch seconds in UTC (INTEGER) throughout. A local-time
-- string never enters the database; conversion happens at the store boundary.

-- ─── Incidents ────────────────────────────────────────────────────────────
--
-- Created before alerts because alerts reference it.

CREATE TABLE incidents (
    id                  INTEGER PRIMARY KEY,
    group_key           TEXT    NOT NULL,
    team                TEXT    NOT NULL,
    policy              TEXT    NOT NULL,
    -- The escalation policy structure as it stood when this incident opened,
    -- serialized as JSON. An incident escalates the way it started even if a
    -- config reload alters or deletes the policy; targets are still resolved
    -- live at step-fire time. See docs/DECISIONS.md D4.
    policy_snapshot     TEXT    NOT NULL DEFAULT '',
    severity            TEXT    NOT NULL,
    title               TEXT    NOT NULL,
    status              TEXT    NOT NULL,
    current_step        INTEGER NOT NULL DEFAULT 0,
    alert_count         INTEGER NOT NULL DEFAULT 1,
    created_at          INTEGER NOT NULL,
    acknowledged_at     INTEGER,
    acknowledged_by     TEXT,
    resolved_at         INTEGER,
    -- User id, or 'auto' when closed by a resolve signal.
    resolved_by         TEXT,
    last_alert_at       INTEGER NOT NULL,
    dedup_count         INTEGER NOT NULL DEFAULT 0
);

-- The enforcement point for deduplication: at most one open incident per group
-- key, as a database invariant rather than application-layer hope.
CREATE UNIQUE INDEX idx_incidents_open_group
    ON incidents(group_key) WHERE status IN ('triggered','acknowledged');

CREATE INDEX idx_incidents_status ON incidents(status, created_at DESC);

-- ─── Alerts ───────────────────────────────────────────────────────────────

CREATE TABLE alerts (
    id              INTEGER PRIMARY KEY,
    fingerprint     TEXT    NOT NULL,
    -- 'alertmanager' | 'grafana' | 'generic' | 'heartbeat'
    source          TEXT    NOT NULL,
    -- 'firing' | 'resolved'
    status          TEXT    NOT NULL,
    labels          TEXT    NOT NULL,   -- JSON object
    annotations     TEXT    NOT NULL,   -- JSON object
    starts_at       INTEGER NOT NULL,
    ends_at         INTEGER,
    received_at     INTEGER NOT NULL,
    incident_id     INTEGER REFERENCES incidents(id),
    raw             TEXT                -- original payload, for debugging
);

CREATE INDEX idx_alerts_fingerprint ON alerts(fingerprint, received_at DESC);
CREATE INDEX idx_alerts_incident    ON alerts(incident_id);

-- ─── Durable timers ───────────────────────────────────────────────────────

CREATE TABLE timers (
    id              INTEGER PRIMARY KEY,
    incident_id     INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    -- 'escalate' | 'group_wait' | 'resolve_timeout' | 'repeat'
    kind            TEXT    NOT NULL,
    fire_at         INTEGER NOT NULL,
    payload         TEXT,               -- JSON
    created_at      INTEGER NOT NULL,
    -- Unused in v1. Reserved for the lease-based claim that leader election
    -- would require, so adding HA needs no migration. A timer's effect is a
    -- pure database state change applied in the same transaction that sets
    -- completed_at, which is what makes exactly-once execution follow from
    -- SQLite atomicity rather than application logic. See DECISIONS D1.
    claimed_at      INTEGER,
    completed_at    INTEGER,
    cancelled_at    INTEGER
);

CREATE INDEX idx_timers_pending ON timers(fire_at)
    WHERE completed_at IS NULL AND cancelled_at IS NULL;

-- ─── Notification outbox ──────────────────────────────────────────────────

CREATE TABLE notifications (
    id                  INTEGER PRIMARY KEY,
    incident_id         INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    -- sha256(incident_id | step_index | target_user | channel | attempt_group).
    -- UNIQUE, so a retried escalation cannot enqueue a duplicate page: the
    -- insert conflicts and is ignored.
    idempotency_key     TEXT    NOT NULL UNIQUE,
    step_index          INTEGER NOT NULL,
    target_user         TEXT    NOT NULL,
    -- 'ntfy'|'telegram'|'email'|'webhook'|'sms'|'voice'
    channel             TEXT    NOT NULL,
    -- Address resolved at send time, not at enqueue time.
    destination         TEXT    NOT NULL,
    body                TEXT    NOT NULL,
    -- 'pending'|'sending'|'sent'|'failed'|'dead'
    state               TEXT    NOT NULL,
    attempts            INTEGER NOT NULL DEFAULT 0,
    next_attempt_at     INTEGER,
    last_error          TEXT,
    created_at          INTEGER NOT NULL,
    sent_at             INTEGER
);

CREATE INDEX idx_notifications_due ON notifications(next_attempt_at)
    WHERE state IN ('pending','failed');

-- Reclaiming rows stuck in 'sending' after a crash needs this. See DECISIONS D7.
CREATE INDEX idx_notifications_sending ON notifications(created_at)
    WHERE state = 'sending';

-- ─── Acknowledgements ─────────────────────────────────────────────────────

CREATE TABLE acks (
    id              INTEGER PRIMARY KEY,
    incident_id     INTEGER NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    user_id         TEXT    NOT NULL,
    -- 'link'|'api'|'ui'|'telegram_button'
    via             TEXT    NOT NULL,
    created_at      INTEGER NOT NULL
);

CREATE INDEX idx_acks_incident ON acks(incident_id, created_at);

-- ─── Schedule overrides (state, not config) ───────────────────────────────

CREATE TABLE overrides (
    id              INTEGER PRIMARY KEY,
    schedule_name   TEXT    NOT NULL,
    user_id         TEXT    NOT NULL,   -- who is covering
    starts_at       INTEGER NOT NULL,
    ends_at         INTEGER NOT NULL,
    reason          TEXT,
    created_at      INTEGER NOT NULL,
    created_by      TEXT    NOT NULL
);

CREATE INDEX idx_overrides_window ON overrides(schedule_name, starts_at, ends_at);

-- ─── Heartbeats ───────────────────────────────────────────────────────────

CREATE TABLE heartbeats (
    id                  INTEGER PRIMARY KEY,
    name                TEXT    NOT NULL UNIQUE,
    token               TEXT    NOT NULL UNIQUE,
    expected_interval   INTEGER NOT NULL,   -- seconds
    grace_period        INTEGER NOT NULL,   -- seconds
    team                TEXT    NOT NULL,
    severity            TEXT    NOT NULL,
    last_ping_at        INTEGER,
    -- 'healthy'|'missing'|'never_seen'
    state               TEXT    NOT NULL,
    created_at          INTEGER NOT NULL
);

-- The sweeper scans for overdue heartbeats every 30 seconds.
CREATE INDEX idx_heartbeats_state ON heartbeats(state, last_ping_at);

-- ─── Audit trail ──────────────────────────────────────────────────────────

CREATE TABLE events (
    id              INTEGER PRIMARY KEY,
    incident_id     INTEGER REFERENCES incidents(id) ON DELETE CASCADE,
    -- 'created'|'escalated'|'notified'|'acked'|'resolved'|...
    kind            TEXT    NOT NULL,
    detail          TEXT,               -- JSON
    created_at      INTEGER NOT NULL
);

CREATE INDEX idx_events_incident ON events(incident_id, created_at);
