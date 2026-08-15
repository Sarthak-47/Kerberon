// Package core holds the domain types shared across Kerberon. It depends on
// nothing inside the project, so no package needs to import store merely to name
// a struct. See docs/DECISIONS.md D8.
//
// Times are time.Time in Go and Unix epoch seconds in UTC in SQLite. The
// conversion happens at the store boundary and nowhere else; the database never
// holds a local-time string.
package core

import (
	"fmt"
	"time"
)

// ─── Enumerations ─────────────────────────────────────────────────────────
//
// These mirror the CHECK-free TEXT columns in the schema. Validity is enforced
// in Go rather than by SQLite constraints so that a bad value produces a useful
// error rather than a generic constraint failure.

// Source identifies which ingest path produced an alert.
type Source string

const (
	SourceAlertmanager Source = "alertmanager"
	SourceGrafana      Source = "grafana"
	SourceGeneric      Source = "generic"
	SourceHeartbeat    Source = "heartbeat"
)

func (s Source) Valid() bool {
	switch s {
	case SourceAlertmanager, SourceGrafana, SourceGeneric, SourceHeartbeat:
		return true
	}
	return false
}

// AlertStatus is the firing state of a single alert.
type AlertStatus string

const (
	AlertFiring   AlertStatus = "firing"
	AlertResolved AlertStatus = "resolved"
)

func (s AlertStatus) Valid() bool {
	return s == AlertFiring || s == AlertResolved
}

// Severity is the urgency carried by an incident.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// IncidentStatus is a node in the escalation state machine (spec §8.1).
type IncidentStatus string

const (
	// IncidentTriggered means firing and not yet acknowledged. Escalation
	// timers are pending.
	IncidentTriggered IncidentStatus = "triggered"
	// IncidentAcknowledged means a human took it. Escalation timers are
	// cancelled, but the incident is not resolved.
	IncidentAcknowledged IncidentStatus = "acknowledged"
	IncidentResolved     IncidentStatus = "resolved"
	// IncidentExpired means escalation exhausted every step and repeat without
	// an acknowledgement. Nobody answered, which is critical information and is
	// surfaced loudly rather than dropped.
	IncidentExpired    IncidentStatus = "expired"
	IncidentSuppressed IncidentStatus = "suppressed"
)

func (s IncidentStatus) Valid() bool {
	switch s {
	case IncidentTriggered, IncidentAcknowledged, IncidentResolved,
		IncidentExpired, IncidentSuppressed:
		return true
	}
	return false
}

// Open reports whether the incident still occupies its group key. The partial
// unique index on incidents(group_key) covers exactly these two states, making
// "one open incident per group" a database invariant rather than application
// hope (spec §5).
func (s IncidentStatus) Open() bool {
	return s == IncidentTriggered || s == IncidentAcknowledged
}

// TimerKind identifies which handler executes a due timer. The timer package is
// generic and dispatches on this value (D8).
type TimerKind string

const (
	// TimerEscalate advances an incident to its next escalation step.
	TimerEscalate TimerKind = "escalate"
	// TimerGroupWait ends the group_wait window and releases the first page.
	TimerGroupWait TimerKind = "group_wait"
	// TimerResolveTimeout closes an incident once resolve_grace has passed
	// without a re-fire.
	TimerResolveTimeout TimerKind = "resolve_timeout"
	// TimerRepeat loops an escalation policy that has run out of steps.
	TimerRepeat TimerKind = "repeat"
)

func (k TimerKind) Valid() bool {
	switch k {
	case TimerEscalate, TimerGroupWait, TimerResolveTimeout, TimerRepeat:
		return true
	}
	return false
}

// NotificationState tracks an outbox row through delivery.
type NotificationState string

const (
	NotifPending NotificationState = "pending"
	// NotifSending means a worker claimed it and may have already made the
	// outbound call. After a crash this state is ambiguous, and the row is
	// retried once its lease expires: a duplicate page beats a missed one (D7).
	NotifSending NotificationState = "sending"
	NotifSent    NotificationState = "sent"
	NotifFailed  NotificationState = "failed"
	// NotifDead means every retry was exhausted. This triggers dead-letter
	// escalation — the paging system failing to page is itself an incident.
	NotifDead NotificationState = "dead"
)

func (s NotificationState) Valid() bool {
	switch s {
	case NotifPending, NotifSending, NotifSent, NotifFailed, NotifDead:
		return true
	}
	return false
}

// Channel is a delivery mechanism. Twilio's sms and voice are defined here but
// not implemented until v1.1 (spec §8.5).
type Channel string

const (
	ChannelNtfy     Channel = "ntfy"
	ChannelTelegram Channel = "telegram"
	ChannelEmail    Channel = "email"
	ChannelWebhook  Channel = "webhook"
	ChannelSMS      Channel = "sms"
	ChannelVoice    Channel = "voice"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelNtfy, ChannelTelegram, ChannelEmail, ChannelWebhook,
		ChannelSMS, ChannelVoice:
		return true
	}
	return false
}

// AckVia records how an acknowledgement arrived.
type AckVia string

const (
	AckViaLink           AckVia = "link"
	AckViaAPI            AckVia = "api"
	AckViaUI             AckVia = "ui"
	AckViaTelegramButton AckVia = "telegram_button"
)

func (v AckVia) Valid() bool {
	switch v {
	case AckViaLink, AckViaAPI, AckViaUI, AckViaTelegramButton:
		return true
	}
	return false
}

// HeartbeatState is the liveness of a dead-man's switch.
type HeartbeatState string

const (
	HeartbeatHealthy   HeartbeatState = "healthy"
	HeartbeatMissing   HeartbeatState = "missing"
	HeartbeatNeverSeen HeartbeatState = "never_seen"
)

func (s HeartbeatState) Valid() bool {
	switch s {
	case HeartbeatHealthy, HeartbeatMissing, HeartbeatNeverSeen:
		return true
	}
	return false
}

// EventKind labels an entry in an incident's timeline.
type EventKind string

const (
	EventCreated    EventKind = "created"
	EventEscalated  EventKind = "escalated"
	EventNotified   EventKind = "notified"
	EventAcked      EventKind = "acked"
	EventResolved   EventKind = "resolved"
	EventExpired    EventKind = "expired"
	EventGrouped    EventKind = "grouped"
	EventSuppressed EventKind = "suppressed"
)

// ─── Records ──────────────────────────────────────────────────────────────

// Labels are the key/value pairs an alert carries. Stored as a JSON object.
type Labels map[string]string

// Annotations are the human-facing fields of an alert (summary, description).
type Annotations map[string]string

// Alert is one firing or resolved signal from a monitoring source.
type Alert struct {
	ID          int64
	Fingerprint string
	Source      Source
	Status      AlertStatus
	Labels      Labels
	Annotations Annotations
	StartsAt    time.Time
	EndsAt      *time.Time
	ReceivedAt  time.Time
	IncidentID  *int64
	// Raw is the original payload, kept for debugging a misbehaving source.
	Raw string
}

// Incident is a group of related alerts and the unit a human is paged about.
type Incident struct {
	ID       int64
	GroupKey string
	Team     string
	Policy   string
	Severity Severity
	Title    string
	Status   IncidentStatus
	// PolicySnapshot is the escalation policy structure as it stood when the
	// incident opened, serialized as JSON. An incident escalates the way it
	// started even if a config reload alters or removes the policy; targets are
	// still resolved live at step-fire time (D4).
	PolicySnapshot string
	CurrentStep    int
	AlertCount     int
	CreatedAt      time.Time
	AcknowledgedAt *time.Time
	AcknowledgedBy string
	ResolvedAt     *time.Time
	// ResolvedBy is a user id, or "auto" when closed by a resolve signal.
	ResolvedBy  string
	LastAlertAt time.Time
	DedupCount  int
}

// Timer is a durable future action. Its effect must be a pure database state
// change applied in the same transaction that marks it complete (D1).
type Timer struct {
	ID         int64
	IncidentID int64
	Kind       TimerKind
	FireAt     time.Time
	// Payload is handler-specific JSON.
	Payload   string
	CreatedAt time.Time
	// ClaimedAt is unused in v1. It is reserved for the lease-based claim that
	// leader election would require, so adding HA needs no migration (D1).
	ClaimedAt   *time.Time
	CompletedAt *time.Time
	CancelledAt *time.Time
}

// Pending reports whether the timer is still eligible to fire.
func (t Timer) Pending() bool {
	return t.CompletedAt == nil && t.CancelledAt == nil
}

// Notification is an outbox row. It is written in the same transaction that
// advances incident state, so a crash between "state advanced" and "page sent"
// cannot lose the page (spec §8.2).
type Notification struct {
	ID         int64
	IncidentID int64
	// IdempotencyKey is UNIQUE. A retried escalation cannot enqueue a duplicate
	// notification: the insert simply conflicts and is ignored.
	IdempotencyKey string
	StepIndex      int
	TargetUser     string
	Channel        Channel
	// Destination is the address resolved at send time, not at enqueue time.
	Destination   string
	Body          string
	State         NotificationState
	Attempts      int
	NextAttemptAt *time.Time
	LastError     string
	CreatedAt     time.Time
	SentAt        *time.Time
}

// Ack records an acknowledgement.
type Ack struct {
	ID         int64
	IncidentID int64
	UserID     string
	Via        AckVia
	CreatedAt  time.Time
}

// Override is a human covering a specific window. Overrides are state rather
// than config, because forcing a git commit for a last-minute swap would be
// user-hostile (spec §4.2).
type Override struct {
	ID           int64
	ScheduleName string
	UserID       string
	StartsAt     time.Time
	EndsAt       time.Time
	Reason       string
	CreatedAt    time.Time
	CreatedBy    string
}

// Heartbeat is a dead-man's switch: if a ping does not arrive within
// ExpectedInterval + GracePeriod, Kerberon raises an incident.
type Heartbeat struct {
	ID               int64
	Name             string
	Token            string
	ExpectedInterval time.Duration
	GracePeriod      time.Duration
	Team             string
	Severity         Severity
	LastPingAt       *time.Time
	State            HeartbeatState
	CreatedAt        time.Time
}

// Due reports whether the heartbeat is overdue at time now.
func (h Heartbeat) Due(now time.Time) bool {
	if h.LastPingAt == nil {
		return false // never_seen is handled separately; absence is not lateness
	}
	return now.After(h.LastPingAt.Add(h.ExpectedInterval + h.GracePeriod))
}

// Event is one entry in an incident's audit timeline.
type Event struct {
	ID         int64
	IncidentID *int64
	Kind       EventKind
	// Detail is kind-specific JSON.
	Detail    string
	CreatedAt time.Time
}

// ─── Scheduling ───────────────────────────────────────────────────────────

// Interval is a contiguous span during which exactly one user is on call. It is
// the primitive of schedule resolution: point lookups are derived from
// intervals, not the other way round, because gap detection and the 400-day
// no-gap property test cannot be built correctly from sampling (D6).
type Interval struct {
	Start  time.Time
	End    time.Time // exclusive
	UserID string
}

// Contains reports whether t falls in [Start, End).
func (i Interval) Contains(t time.Time) bool {
	return !t.Before(i.Start) && t.Before(i.End)
}

// Duration is the length of the interval.
func (i Interval) Duration() time.Duration { return i.End.Sub(i.Start) }

func (i Interval) String() string {
	return fmt.Sprintf("%s → %s: %s",
		i.Start.Format(time.RFC3339), i.End.Format(time.RFC3339), i.UserID)
}
