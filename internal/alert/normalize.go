package alert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// MaxAlertsPerPayload bounds a single request. Alertmanager batches a group
// into one webhook, so a large cascade legitimately arrives together, but an
// unbounded payload is a denial-of-service vector on a service whose whole job
// is to stay up.
const MaxAlertsPerPayload = 5000

// ─── Alertmanager ─────────────────────────────────────────────────────────

// alertmanagerPayload is the Prometheus Alertmanager webhook format (v4).
type alertmanagerPayload struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     string            `json:"startsAt"`
		EndsAt       string            `json:"endsAt"`
		GeneratorURL string            `json:"generatorURL"`
		Fingerprint  string            `json:"fingerprint"`
	} `json:"alerts"`
}

// ParseAlertmanager normalizes an Alertmanager webhook body.
//
// receivedAt is supplied by the caller from a Clock rather than read here, so
// ingest stays testable.
func ParseAlertmanager(body []byte, receivedAt time.Time) ([]core.Alert, error) {
	var p alertmanagerPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parse alertmanager payload: %w", err)
	}
	if len(p.Alerts) == 0 {
		return nil, fmt.Errorf("alertmanager payload contains no alerts")
	}
	if len(p.Alerts) > MaxAlertsPerPayload {
		return nil, fmt.Errorf("alertmanager payload contains %d alerts, limit is %d",
			len(p.Alerts), MaxAlertsPerPayload)
	}

	out := make([]core.Alert, 0, len(p.Alerts))
	for i, raw := range p.Alerts {
		// Alertmanager sends commonLabels separately from per-alert labels;
		// merge so each normalized alert carries its full label set.
		labels := merge(p.CommonLabels, raw.Labels)
		annotations := merge(p.CommonAnnotations, raw.Annotations)

		status := core.AlertStatus(strings.ToLower(raw.Status))
		if status == "" {
			status = core.AlertStatus(strings.ToLower(p.Status))
		}
		if !status.Valid() {
			return nil, fmt.Errorf("alert %d has unknown status %q", i, raw.Status)
		}

		startsAt, err := parseTime(raw.StartsAt, receivedAt)
		if err != nil {
			return nil, fmt.Errorf("alert %d startsAt: %w", i, err)
		}
		endsAt, err := parseOptionalTime(raw.EndsAt)
		if err != nil {
			return nil, fmt.Errorf("alert %d endsAt: %w", i, err)
		}

		if raw.GeneratorURL != "" {
			annotations = withKey(annotations, "generator_url", raw.GeneratorURL)
		}

		out = append(out, core.Alert{
			Source:      core.SourceAlertmanager,
			Status:      status,
			Labels:      core.Labels(labels),
			Annotations: core.Annotations(annotations),
			StartsAt:    startsAt,
			EndsAt:      endsAt,
			ReceivedAt:  receivedAt,
		})
	}
	return out, nil
}

// ParseGrafana normalizes a Grafana unified-alerting webhook.
//
// Grafana emits the Alertmanager webhook shape, so the parser is shared and
// only the recorded source differs. Keeping a separate entry point means a
// future divergence needs no change at the call site.
func ParseGrafana(body []byte, receivedAt time.Time) ([]core.Alert, error) {
	alerts, err := ParseAlertmanager(body, receivedAt)
	if err != nil {
		return nil, fmt.Errorf("parse grafana payload: %w", unwrapPrefix(err, "parse alertmanager payload: "))
	}
	for i := range alerts {
		alerts[i].Source = core.SourceGrafana
	}
	return alerts, nil
}

// ─── Generic ──────────────────────────────────────────────────────────────

// genericPayload is Kerberon's own format: either a single alert or a list.
type genericPayload struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	EndsAt      string            `json:"endsAt"`
	Alerts      []genericPayload  `json:"alerts"`
}

// ParseGeneric normalizes Kerberon's own alert format. It accepts a single
// alert object or an object with an "alerts" array, because both shapes are
// what people actually send from a shell script.
func ParseGeneric(body []byte, receivedAt time.Time) ([]core.Alert, error) {
	var p genericPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("parse generic payload: %w", err)
	}

	raws := p.Alerts
	if len(raws) == 0 {
		// A bare single alert.
		raws = []genericPayload{p}
	}
	if len(raws) > MaxAlertsPerPayload {
		return nil, fmt.Errorf("payload contains %d alerts, limit is %d",
			len(raws), MaxAlertsPerPayload)
	}

	out := make([]core.Alert, 0, len(raws))
	for i, raw := range raws {
		if len(raw.Labels) == 0 {
			return nil, fmt.Errorf("alert %d has no labels; at least alertname is required", i)
		}

		status := core.AlertStatus(strings.ToLower(strings.TrimSpace(raw.Status)))
		if status == "" {
			// Omitting status means firing: that is what a script posting an
			// alert intends, and defaulting to resolved would silently close
			// incidents.
			status = core.AlertFiring
		}
		if !status.Valid() {
			return nil, fmt.Errorf("alert %d has unknown status %q", i, raw.Status)
		}

		startsAt, err := parseTime(raw.StartsAt, receivedAt)
		if err != nil {
			return nil, fmt.Errorf("alert %d startsAt: %w", i, err)
		}
		endsAt, err := parseOptionalTime(raw.EndsAt)
		if err != nil {
			return nil, fmt.Errorf("alert %d endsAt: %w", i, err)
		}

		out = append(out, core.Alert{
			Source:      core.SourceGeneric,
			Status:      status,
			Labels:      copyMap(raw.Labels),
			Annotations: copyMap(raw.Annotations),
			StartsAt:    startsAt,
			EndsAt:      endsAt,
			ReceivedAt:  receivedAt,
		})
	}
	return out, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// parseTime accepts RFC3339 and falls back to fallback when empty.
func parseTime(s string, fallback time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not RFC3339", s)
	}
	return t.UTC(), nil
}

// parseOptionalTime returns nil for an absent or zero timestamp.
//
// Alertmanager sends "0001-01-01T00:00:00Z" for an alert that has not ended,
// which must not be stored as a real end time or the alert would look resolved
// in the distant past.
func parseOptionalTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("%q is not RFC3339", s)
	}
	if t.IsZero() || t.Year() <= 1 {
		return nil, nil
	}
	utc := t.UTC()
	return &utc, nil
}

// merge combines maps, with later maps winning. It returns a plain map so the
// caller can convert to Labels or Annotations as appropriate.
func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func withKey(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	if _, exists := m[k]; !exists {
		m[k] = v
	}
	return m
}

// unwrapPrefix strips a wrapping prefix so a nested parser's message does not
// name the wrong format to the operator.
func unwrapPrefix(err error, prefix string) error {
	msg := err.Error()
	if strings.HasPrefix(msg, prefix) {
		return fmt.Errorf("%s", strings.TrimPrefix(msg, prefix))
	}
	return err
}
