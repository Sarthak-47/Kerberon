package alert_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/alert"
	"github.com/Sarthak-47/kerberon/internal/core"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// ─── Fingerprinting ───────────────────────────────────────────────────────

func TestFingerprintIsStableAndOrderIndependent(t *testing.T) {
	a := core.Labels{"alertname": "HighCPU", "cluster": "prod", "service": "api"}
	b := core.Labels{"service": "api", "alertname": "HighCPU", "cluster": "prod"}

	if alert.Fingerprint(a, nil) != alert.Fingerprint(b, nil) {
		t.Error("fingerprint depends on map iteration order")
	}
	if alert.Fingerprint(a, nil) != alert.Fingerprint(a, nil) {
		t.Error("fingerprint is not deterministic")
	}
}

func TestFingerprintDistinguishesDifferentLabels(t *testing.T) {
	base := core.Labels{"alertname": "HighCPU", "service": "api"}
	other := core.Labels{"alertname": "HighCPU", "service": "web"}

	if alert.Fingerprint(base, nil) == alert.Fingerprint(other, nil) {
		t.Error("different label values produced the same fingerprint")
	}
}

// Without a separator between entries, {"ab":"c"} and {"a":"bc"} serialize
// identically and two unrelated alerts would dedupe into one incident.
func TestFingerprintIsNotVulnerableToConcatenationCollisions(t *testing.T) {
	one := core.Labels{"ab": "c"}
	two := core.Labels{"a": "bc"}
	if alert.Fingerprint(one, nil) == alert.Fingerprint(two, nil) {
		t.Error("label boundaries are not encoded; unrelated alerts would collide")
	}
}

// The whole point of the volatile set: a rescheduled pod must not look like a
// brand new alert.
func TestVolatileLabelsAreExcluded(t *testing.T) {
	before := core.Labels{"alertname": "PodCrash", "service": "api", "pod": "api-7f9d-abc"}
	after := core.Labels{"alertname": "PodCrash", "service": "api", "pod": "api-7f9d-xyz"}

	if got, want := alert.Fingerprint(after, alert.DefaultVolatileLabels),
		alert.Fingerprint(before, alert.DefaultVolatileLabels); got != want {
		t.Error("a rescheduled pod produced a new fingerprint; this is the most common cause of duplicate paging")
	}
	// Without the exclusion they must differ, or the test above proves nothing.
	if alert.Fingerprint(before, nil) == alert.Fingerprint(after, nil) {
		t.Error("pod name is not contributing to the fingerprint at all")
	}
}

func TestVolatileLabelMatchingIsCaseInsensitive(t *testing.T) {
	a := core.Labels{"alertname": "X", "Pod": "one"}
	b := core.Labels{"alertname": "X", "Pod": "two"}
	if alert.Fingerprint(a, []string{"pod"}) != alert.Fingerprint(b, []string{"pod"}) {
		t.Error("volatile label matching should be case-insensitive")
	}
}

func TestEmptyLabelsStillFingerprint(t *testing.T) {
	if got := alert.Fingerprint(core.Labels{}, nil); len(got) != 64 {
		t.Errorf("fingerprint = %q, want 64 hex characters", got)
	}
}

// ─── Group keys ───────────────────────────────────────────────────────────

func TestGroupKeyGroupsMatchingAlerts(t *testing.T) {
	groupBy := []string{"alertname", "cluster"}
	one := core.Labels{"alertname": "HighCPU", "cluster": "prod", "pod": "a"}
	two := core.Labels{"alertname": "HighCPU", "cluster": "prod", "pod": "b"}
	three := core.Labels{"alertname": "HighCPU", "cluster": "staging"}

	if alert.GroupKey("critical", groupBy, one) != alert.GroupKey("critical", groupBy, two) {
		t.Error("alerts sharing group_by values should share a group key")
	}
	if alert.GroupKey("critical", groupBy, one) == alert.GroupKey("critical", groupBy, three) {
		t.Error("a differing group_by value should produce a different group key")
	}
}

// Two routes must not share an incident even with identical labels, or one
// team's alert could be attached to another team's incident.
func TestGroupKeyIsScopedToTheRoute(t *testing.T) {
	groupBy := []string{"alertname"}
	labels := core.Labels{"alertname": "HighCPU"}
	if alert.GroupKey("route-a", groupBy, labels) == alert.GroupKey("route-b", groupBy, labels) {
		t.Error("group keys collide across routes")
	}
}

func TestGroupKeyIgnoresGroupByOrdering(t *testing.T) {
	labels := core.Labels{"alertname": "X", "cluster": "prod"}
	if alert.GroupKey("r", []string{"alertname", "cluster"}, labels) !=
		alert.GroupKey("r", []string{"cluster", "alertname"}, labels) {
		t.Error("group key depends on how group_by happens to be ordered in config")
	}
}

// An alert missing a grouping label must still group with its peers rather
// than forming a singleton incident.
func TestMissingGroupByLabelGroupsAsEmpty(t *testing.T) {
	groupBy := []string{"alertname", "cluster"}
	a := core.Labels{"alertname": "X"}
	b := core.Labels{"alertname": "X"}
	if alert.GroupKey("r", groupBy, a) != alert.GroupKey("r", groupBy, b) {
		t.Error("alerts both missing a group_by label should still group together")
	}
}

// ─── Title and severity ───────────────────────────────────────────────────

func TestTitlePrefersTheHumanWrittenSummary(t *testing.T) {
	a := core.Alert{
		Labels:      core.Labels{"alertname": "HighCPU"},
		Annotations: core.Annotations{"summary": "API latency above 2s"},
	}
	if got := alert.Title(a); got != "API latency above 2s" {
		t.Errorf("Title = %q, want the summary annotation", got)
	}
}

func TestTitleFallsBackToAlertname(t *testing.T) {
	a := core.Alert{Labels: core.Labels{"alertname": "HighCPU"}}
	if got := alert.Title(a); got != "HighCPU" {
		t.Errorf("Title = %q, want HighCPU", got)
	}
}

func TestTitleHasAFinalFallback(t *testing.T) {
	if got := alert.Title(core.Alert{}); got == "" {
		t.Error("Title should never be empty; the UI would show a blank incident")
	}
}

func TestTitleTruncatesWithoutSplittingRunes(t *testing.T) {
	long := strings.Repeat("é", 300)
	a := core.Alert{Annotations: core.Annotations{"summary": long}}
	got := alert.Title(a)
	if len(got) > 210 {
		t.Errorf("title length %d, want it truncated", len(got))
	}
	// Invalid UTF-8 would render as a replacement character in the UI.
	for i, r := range got {
		if r == '�' {
			t.Fatalf("truncation split a rune at byte %d", i)
		}
	}
}

func TestSeverityFromLabels(t *testing.T) {
	cases := []struct {
		labels core.Labels
		want   core.Severity
	}{
		{core.Labels{"severity": "critical"}, core.SeverityCritical},
		{core.Labels{"severity": "warning"}, core.SeverityWarning},
		{core.Labels{"severity": "info"}, core.SeverityInfo},
		{core.Labels{"severity": "CRITICAL"}, core.SeverityCritical},
		{core.Labels{"severity": "page"}, core.SeverityCritical},
		{core.Labels{"severity": "warn"}, core.SeverityWarning},
		{core.Labels{"priority": "P1"}, core.SeverityCritical},
		// Unlabelled defaults upward: an alert that turns out to matter is
		// better paged than silently downgraded.
		{core.Labels{}, core.SeverityCritical},
		{core.Labels{"severity": "nonsense"}, core.SeverityCritical},
	}
	for _, c := range cases {
		if got := alert.Severity(c.labels); got != c.want {
			t.Errorf("Severity(%v) = %q, want %q", c.labels, got, c.want)
		}
	}
}

// ─── Alertmanager ─────────────────────────────────────────────────────────

const alertmanagerBody = `{
  "version": "4",
  "groupKey": "{}:{alertname=\"HighCPU\"}",
  "status": "firing",
  "receiver": "kerberon",
  "groupLabels": {"alertname": "HighCPU"},
  "commonLabels": {"alertname": "HighCPU", "cluster": "prod"},
  "commonAnnotations": {"runbook": "https://wiki/runbook"},
  "externalURL": "https://alertmanager.example.com",
  "alerts": [
    {
      "status": "firing",
      "labels": {"service": "api", "severity": "critical"},
      "annotations": {"summary": "API is down"},
      "startsAt": "2026-08-15T09:00:00Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "https://prometheus/graph?g0.expr=up",
      "fingerprint": "abc123"
    },
    {
      "status": "resolved",
      "labels": {"service": "web"},
      "annotations": {},
      "startsAt": "2026-08-15T08:00:00Z",
      "endsAt": "2026-08-15T09:30:00Z"
    }
  ]
}`

func TestParseAlertmanager(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	alerts, err := alert.ParseAlertmanager([]byte(alertmanagerBody), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(alerts))
	}

	first := alerts[0]
	if first.Source != core.SourceAlertmanager {
		t.Errorf("source = %q", first.Source)
	}
	if first.Status != core.AlertFiring {
		t.Errorf("status = %q, want firing", first.Status)
	}
	// commonLabels must be merged into each alert's own labels.
	for k, want := range map[string]string{
		"alertname": "HighCPU", "cluster": "prod", "service": "api", "severity": "critical",
	} {
		if got := first.Labels[k]; got != want {
			t.Errorf("label %q = %q, want %q", k, got, want)
		}
	}
	if got := first.Annotations["runbook"]; got != "https://wiki/runbook" {
		t.Errorf("common annotation not merged: %q", got)
	}
	if got := first.Annotations["generator_url"]; got == "" {
		t.Error("generatorURL should be preserved for the incident timeline")
	}
	if !first.StartsAt.Equal(at(t, "2026-08-15T09:00:00Z")) {
		t.Errorf("startsAt = %v", first.StartsAt)
	}
	// Alertmanager sends a zero endsAt for an ongoing alert; storing it would
	// make the alert look resolved in the year 1.
	if first.EndsAt != nil {
		t.Errorf("endsAt = %v, want nil for an ongoing alert", *first.EndsAt)
	}
	if !first.ReceivedAt.Equal(now) {
		t.Errorf("receivedAt = %v, want the supplied clock time", first.ReceivedAt)
	}

	second := alerts[1]
	if second.Status != core.AlertResolved {
		t.Errorf("second alert status = %q, want resolved", second.Status)
	}
	if second.EndsAt == nil {
		t.Fatal("resolved alert should carry an endsAt")
	}
}

func TestParseAlertmanagerRejectsBadPayloads(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	cases := []struct {
		name string
		body string
	}{
		{"not json", `nonsense`},
		{"no alerts", `{"version":"4","alerts":[]}`},
		{"unknown status", `{"alerts":[{"status":"maybe","labels":{"a":"b"}}]}`},
		{"bad startsAt", `{"alerts":[{"status":"firing","labels":{"a":"b"},"startsAt":"yesterday"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := alert.ParseAlertmanager([]byte(c.body), now); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseGrafanaRecordsItsOwnSource(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	alerts, err := alert.ParseGrafana([]byte(alertmanagerBody), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, a := range alerts {
		if a.Source != core.SourceGrafana {
			t.Errorf("source = %q, want grafana", a.Source)
		}
	}
}

// ─── Generic ──────────────────────────────────────────────────────────────

func TestParseGenericSingleAlert(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	body := `{"labels":{"alertname":"DiskFull","service":"db"},"annotations":{"summary":"disk at 95%"}}`

	alerts, err := alert.ParseGeneric([]byte(body), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("got %d alerts, want 1", len(alerts))
	}
	a := alerts[0]
	if a.Source != core.SourceGeneric {
		t.Errorf("source = %q", a.Source)
	}
	// Omitting status means firing. Defaulting to resolved would silently
	// close incidents.
	if a.Status != core.AlertFiring {
		t.Errorf("status = %q, want firing by default", a.Status)
	}
	// Omitting startsAt means now.
	if !a.StartsAt.Equal(now) {
		t.Errorf("startsAt = %v, want the receive time", a.StartsAt)
	}
}

func TestParseGenericAlertsArray(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	body := `{"alerts":[
		{"labels":{"alertname":"A"}},
		{"labels":{"alertname":"B"},"status":"resolved","endsAt":"2026-08-15T09:00:00Z"}
	]}`

	alerts, err := alert.ParseGeneric([]byte(body), now)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want 2", len(alerts))
	}
	if alerts[1].Status != core.AlertResolved || alerts[1].EndsAt == nil {
		t.Error("second alert should be resolved with an end time")
	}
}

func TestParseGenericRequiresLabels(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	if _, err := alert.ParseGeneric([]byte(`{"annotations":{"summary":"x"}}`), now); err == nil {
		t.Fatal("an alert with no labels should be rejected; it could not be routed or grouped")
	}
}

func TestParsersRejectOversizedPayloads(t *testing.T) {
	now := at(t, "2026-08-15T10:00:00Z")
	var b strings.Builder
	b.WriteString(`{"alerts":[`)
	for i := 0; i < alert.MaxAlertsPerPayload+1; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"status":"firing","labels":{"alertname":"X"}}`)
	}
	b.WriteString(`]}`)

	if _, err := alert.ParseGeneric([]byte(b.String()), now); err == nil {
		t.Fatal("an oversized payload should be rejected")
	}
}
