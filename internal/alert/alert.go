// Package alert normalizes incoming payloads into Kerberon's internal Alert
// and computes the fingerprint used for deduplication.
//
// Adding a monitoring source means writing one normalizer here and nothing
// else: the grouping engine, router and escalation engine all work on the
// normalized form (spec section 6.1).
package alert

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// DefaultVolatileLabels are excluded from the fingerprint unless a route
// overrides them.
//
// The set exists because including a pod name means a rescheduled pod produces
// a "new" alert every time, which is the single most common cause of duplicate
// paging in Kubernetes (spec section 6.2).
var DefaultVolatileLabels = []string{
	"timestamp", "value", "instance_id", "pod", "container_id", "trace_id",
}

// Fingerprint identifies an alert independently of its volatile labels.
//
//	fingerprint = hex(sha256(canonical_labels))
//
// canonical_labels is every label except those named in volatile, sorted by
// key and serialized as k=v\x00k=v\x00...
//
// The separator matters: without it, {"ab": "c"} and {"a": "bc"} would produce
// identical input and collide.
func Fingerprint(labels core.Labels, volatile []string) string {
	excluded := make(map[string]bool, len(volatile))
	for _, v := range volatile {
		excluded[strings.ToLower(v)] = true
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		if excluded[strings.ToLower(k)] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte("="))
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// GroupKey identifies the incident an alert belongs to.
//
//	group_key = sha256(route_name + sorted(group_by label values))
//
// Alerts sharing values for the group_by labels join the same incident. A label
// missing from an alert contributes an empty value, so alerts that lack a
// grouping label still group together rather than each forming their own
// incident.
func GroupKey(routeName string, groupBy []string, labels core.Labels) string {
	// group_by is ordered by the operator, but sorting makes the key
	// independent of how the config happens to list them.
	keys := make([]string, len(groupBy))
	copy(keys, groupBy)
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(routeName))
	h.Write([]byte{0})
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte("="))
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Title builds a human-readable incident title from an alert.
//
// It prefers an explicit summary annotation, since that is what the alert
// author wrote for a human to read at 3am, and falls back to the alertname
// label and then to a generic string.
func Title(a core.Alert) string {
	for _, key := range []string{"summary", "title", "description"} {
		if v := strings.TrimSpace(a.Annotations[key]); v != "" {
			return truncate(v, 200)
		}
	}
	if v := strings.TrimSpace(a.Labels["alertname"]); v != "" {
		return truncate(v, 200)
	}
	return "Alert"
}

// Severity extracts an incident severity from an alert's labels, defaulting to
// critical. Defaulting upward is deliberate: an unlabelled alert that turns out
// to matter is better paged than silently downgraded.
func Severity(labels core.Labels) core.Severity {
	for _, key := range []string{"severity", "priority", "level"} {
		v := core.Severity(strings.ToLower(strings.TrimSpace(labels[key])))
		if v.Valid() {
			return v
		}
		// Common synonyms seen in the wild.
		switch strings.ToLower(strings.TrimSpace(labels[key])) {
		case "error", "fatal", "page", "p1", "high":
			return core.SeverityCritical
		case "warn", "p2", "medium":
			return core.SeverityWarning
		case "notice", "debug", "p3", "low":
			return core.SeverityInfo
		}
	}
	return core.SeverityCritical
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Trim on a rune boundary so a multi-byte character is not cut in half.
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "..."
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
