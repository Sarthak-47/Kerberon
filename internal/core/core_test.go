package core_test

import (
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// Open defines which statuses the partial unique index covers. If this drifts
// from the index predicate in 0001_initial.sql, deduplication silently breaks.
func TestIncidentStatusOpenMatchesTheIndexPredicate(t *testing.T) {
	open := map[core.IncidentStatus]bool{
		core.IncidentTriggered:    true,
		core.IncidentAcknowledged: true,
		core.IncidentResolved:     false,
		core.IncidentExpired:      false,
		core.IncidentSuppressed:   false,
	}
	for status, want := range open {
		if got := status.Open(); got != want {
			t.Errorf("%s.Open() = %v, want %v", status, got, want)
		}
	}
}

func TestEnumValidity(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"Source alertmanager", core.SourceAlertmanager.Valid(), true},
		{"Source bogus", core.Source("nagios").Valid(), false},
		{"Source empty", core.Source("").Valid(), false},

		{"AlertStatus firing", core.AlertFiring.Valid(), true},
		{"AlertStatus bogus", core.AlertStatus("pending").Valid(), false},

		{"Severity critical", core.SeverityCritical.Valid(), true},
		{"Severity bogus", core.Severity("fatal").Valid(), false},

		{"IncidentStatus expired", core.IncidentExpired.Valid(), true},
		{"IncidentStatus bogus", core.IncidentStatus("closed").Valid(), false},

		{"TimerKind group_wait", core.TimerGroupWait.Valid(), true},
		{"TimerKind bogus", core.TimerKind("retry").Valid(), false},

		{"NotificationState dead", core.NotifDead.Valid(), true},
		{"NotificationState bogus", core.NotificationState("queued").Valid(), false},

		{"Channel ntfy", core.ChannelNtfy.Valid(), true},
		{"Channel slack", core.Channel("slack").Valid(), false},

		{"AckVia telegram_button", core.AckViaTelegramButton.Valid(), true},
		{"AckVia bogus", core.AckVia("sms").Valid(), false},

		{"HeartbeatState never_seen", core.HeartbeatNeverSeen.Valid(), true},
		{"HeartbeatState bogus", core.HeartbeatState("late").Valid(), false},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: Valid() = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestTimerPending(t *testing.T) {
	at := mustTime(t, "2026-08-15T09:00:00Z")

	if !(core.Timer{}).Pending() {
		t.Error("a fresh timer should be pending")
	}
	if (core.Timer{CompletedAt: &at}).Pending() {
		t.Error("a completed timer should not be pending")
	}
	if (core.Timer{CancelledAt: &at}).Pending() {
		t.Error("a cancelled timer should not be pending")
	}
	// Claimed but not completed is still pending: v1 does not use claimed_at,
	// and a crash after claiming must not strand the timer (D1).
	if !(core.Timer{ClaimedAt: &at}).Pending() {
		t.Error("a claimed but incomplete timer should still be pending")
	}
}

// End is exclusive. Two adjacent intervals must not both contain the boundary,
// or the 400-day no-overlap property test would be unsatisfiable.
func TestIntervalContainsIsHalfOpen(t *testing.T) {
	start := mustTime(t, "2026-08-17T09:00:00Z")
	end := mustTime(t, "2026-08-24T09:00:00Z")
	iv := core.Interval{Start: start, End: end, UserID: "sarthak"}

	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before start", start.Add(-time.Second), false},
		{"exactly start", start, true},
		{"midway", start.Add(72 * time.Hour), true},
		{"one before end", end.Add(-time.Second), true},
		{"exactly end", end, false},
		{"after end", end.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := iv.Contains(c.at); got != c.want {
			t.Errorf("%s: Contains(%s) = %v, want %v", c.name, c.at.Format(time.RFC3339), got, c.want)
		}
	}

	if got, want := iv.Duration(), 7*24*time.Hour; got != want {
		t.Errorf("Duration() = %v, want %v", got, want)
	}
}

func TestAdjacentIntervalsDoNotOverlapAtTheBoundary(t *testing.T) {
	handoff := mustTime(t, "2026-08-24T09:00:00Z")
	first := core.Interval{
		Start:  mustTime(t, "2026-08-17T09:00:00Z"),
		End:    handoff,
		UserID: "sarthak",
	}
	second := core.Interval{
		Start:  handoff,
		End:    mustTime(t, "2026-08-31T09:00:00Z"),
		UserID: "priya",
	}
	if first.Contains(handoff) && second.Contains(handoff) {
		t.Fatal("both intervals contain the handoff instant; on-call would be ambiguous")
	}
	if !second.Contains(handoff) {
		t.Error("the incoming interval should own the handoff instant")
	}
}

func TestHeartbeatDue(t *testing.T) {
	last := mustTime(t, "2026-08-15T09:00:00Z")
	hb := core.Heartbeat{
		ExpectedInterval: 5 * time.Minute,
		GracePeriod:      time.Minute,
		LastPingAt:       &last,
	}
	// Due only after expected_interval + grace_period.
	deadline := last.Add(6 * time.Minute)

	if hb.Due(deadline.Add(-time.Second)) {
		t.Error("heartbeat reported overdue before its grace period elapsed")
	}
	if hb.Due(deadline) {
		t.Error("heartbeat reported overdue exactly at the deadline; it should be strictly after")
	}
	if !hb.Due(deadline.Add(time.Second)) {
		t.Error("heartbeat not reported overdue past its grace period")
	}
}

// A heartbeat that has never been pinged is never_seen, a distinct state from
// overdue, so Due must not claim lateness for it.
func TestNeverPingedHeartbeatIsNotDue(t *testing.T) {
	hb := core.Heartbeat{ExpectedInterval: time.Minute, GracePeriod: time.Minute}
	if hb.Due(mustTime(t, "2030-01-01T00:00:00Z")) {
		t.Error("a never-pinged heartbeat should not report as overdue")
	}
}
