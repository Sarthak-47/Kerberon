package route_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/route"
)

// parse builds a Router from YAML, exercising the same path production uses.
func parse(t *testing.T, routesYAML string) *route.Router {
	t.Helper()
	full := `
server:
  external_url: "https://k.example.com"
  secret_key: "s"
  ingest_token: "t"
users:
  - id: sarthak
    name: Sarthak
    timezone: "Asia/Kolkata"
    contacts:
      ntfy: "https://ntfy.sh/x"
teams:
  - name: platform
    members: [sarthak]
  - name: data
    members: [sarthak]
schedules:
  - name: platform-primary
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: base
        type: rotation
        participants: [sarthak]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
escalation_policies:
  - name: critical-24x7
    steps:
      - delay: 0
        targets: [schedule:platform-primary]
        channels: [ntfy]
  - name: business-hours
    steps:
      - delay: 0
        targets: [schedule:platform-primary]
        channels: [ntfy]
channels:
  ntfy:
    default_server: "https://ntfy.sh"
` + routesYAML

	cfg, err := config.Parse([]byte(full), "test.yaml")
	if err != nil {
		t.Fatalf("config:\n%v", err)
	}
	return route.New(cfg)
}

const twoRoutes = `
routes:
  - match:
      severity: critical
      team: platform
    team: platform
    policy: critical-24x7
    group_by: [alertname, cluster]
    group_wait: 30s
  - match:
      severity: warning
    team: platform
    policy: business-hours
    group_by: [alertname]
    group_wait: 2m
`

// ─── Matching ─────────────────────────────────────────────────────────────

func TestFirstMatchWins(t *testing.T) {
	r := parse(t, twoRoutes)

	// Satisfies the first route's criteria, and would also satisfy nothing
	// else; the specific rule above must win.
	got, ok := r.Match(core.Labels{"severity": "critical", "team": "platform"})
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Policy != "critical-24x7" {
		t.Errorf("policy = %q, want critical-24x7", got.Policy)
	}
	if got.GroupWait != 30*time.Second {
		t.Errorf("group_wait = %v, want 30s", got.GroupWait)
	}
}

func TestFallsThroughToLaterRoutes(t *testing.T) {
	r := parse(t, twoRoutes)

	got, ok := r.Match(core.Labels{"severity": "warning", "service": "api"})
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Policy != "business-hours" {
		t.Errorf("policy = %q, want business-hours", got.Policy)
	}
	if got.GroupWait != 2*time.Minute {
		t.Errorf("group_wait = %v, want 2m", got.GroupWait)
	}
}

// Every criterion must hold, not just one.
func TestAllCriteriaMustMatch(t *testing.T) {
	r := parse(t, twoRoutes)

	// severity matches the first route but team does not, so it falls through
	// to the warning route — which it also fails.
	if _, ok := r.Match(core.Labels{"severity": "critical", "team": "data"}); ok {
		t.Error("a partial criteria match should not select the route")
	}
}

// An unmatched alert never pages anyone, which is exactly the failure this
// product exists to prevent. It must be reportable, not silent.
func TestUnmatchedAlertIsReportedNotDropped(t *testing.T) {
	r := parse(t, twoRoutes)

	labels := core.Labels{"severity": "info", "service": "api"}
	if _, ok := r.Match(labels); ok {
		t.Fatal("expected no match")
	}

	err := &route.ErrNoRoute{Labels: labels}
	msg := err.Error()
	for _, want := range []string{"severity", "info", "never page"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message should mention %q, got: %s", want, msg)
		}
	}
}

func TestExtraLabelsDoNotPreventMatching(t *testing.T) {
	r := parse(t, twoRoutes)
	got, ok := r.Match(core.Labels{
		"severity": "warning", "service": "api", "pod": "api-abc", "region": "eu",
	})
	if !ok || got.Policy != "business-hours" {
		t.Errorf("labels beyond the criteria should not prevent a match (ok=%v)", ok)
	}
}

// ─── Route identity ───────────────────────────────────────────────────────

// Reordering routes must not change group keys. If it did, an edit unrelated
// to an open incident would split it in two and page someone again for work
// already in hand.
func TestRouteIdentitySurvivesReordering(t *testing.T) {
	original := parse(t, twoRoutes)

	reordered := parse(t, `
routes:
  - match:
      severity: warning
    team: platform
    policy: business-hours
    group_by: [alertname]
    group_wait: 2m
  - match:
      severity: critical
      team: platform
    team: platform
    policy: critical-24x7
    group_by: [alertname, cluster]
    group_wait: 30s
`)

	before, ok := original.Match(core.Labels{"severity": "warning"})
	if !ok {
		t.Fatal("no match before reordering")
	}
	after, ok := reordered.Match(core.Labels{"severity": "warning"})
	if !ok {
		t.Fatal("no match after reordering")
	}
	if before.Name != after.Name {
		t.Errorf("route name changed on reorder: %q then %q", before.Name, after.Name)
	}

	labels := core.Labels{"alertname": "HighCPU"}
	if before.GroupKey(labels) != after.GroupKey(labels) {
		t.Error("group key changed on reorder; open incidents would split")
	}
}

func TestExplicitNameIsUsed(t *testing.T) {
	r := parse(t, `
routes:
  - name: platform-critical
    match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname]
`)
	got, ok := r.Match(core.Labels{"severity": "critical"})
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Name != "platform-critical" {
		t.Errorf("name = %q, want the configured name", got.Name)
	}
}

// A named route keeps its identity when its match criteria are edited, which
// is the reason to name one.
func TestNamedRouteKeepsIdentityAcrossMatchEdits(t *testing.T) {
	before := parse(t, `
routes:
  - name: platform-critical
    match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname]
`)
	after := parse(t, `
routes:
  - name: platform-critical
    match:
      severity: critical
      env: prod
    team: platform
    policy: critical-24x7
    group_by: [alertname]
`)

	b, _ := before.Match(core.Labels{"severity": "critical"})
	a, _ := after.Match(core.Labels{"severity": "critical", "env": "prod"})

	labels := core.Labels{"alertname": "HighCPU"}
	if b.GroupKey(labels) != a.GroupKey(labels) {
		t.Error("a named route should keep its group identity across match edits")
	}
}

// Two routes must never share an incident, even for identical labels.
func TestDifferentRoutesProduceDifferentGroupKeys(t *testing.T) {
	r := parse(t, twoRoutes)
	routes := r.Routes()
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	labels := core.Labels{"alertname": "HighCPU"}
	if routes[0].GroupKey(labels) == routes[1].GroupKey(labels) {
		t.Error("routes share a group key; one team's alert could join another team's incident")
	}
}

// ─── Defaults carried onto the route ──────────────────────────────────────

func TestRouteCarriesTheEffectiveDefaults(t *testing.T) {
	r := parse(t, `
routes:
  - match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname]
`)
	got, ok := r.Match(core.Labels{"severity": "critical"})
	if !ok {
		t.Fatal("expected a match")
	}
	if got.GroupWait != 30*time.Second {
		t.Errorf("group_wait = %v, want the 30s default", got.GroupWait)
	}
	if got.GroupInterval != 5*time.Minute {
		t.Errorf("group_interval = %v, want the 5m default", got.GroupInterval)
	}
	if got.ResolveGrace != 2*time.Minute {
		t.Errorf("resolve_grace = %v, want the 2m default", got.ResolveGrace)
	}

	// The default volatile set must reach the fingerprint, or a rescheduled
	// pod pages again.
	one := core.Labels{"alertname": "X", "pod": "a"}
	two := core.Labels{"alertname": "X", "pod": "b"}
	if got.Fingerprint(one) != got.Fingerprint(two) {
		t.Error("route fingerprint is not excluding the default volatile labels")
	}
}

func TestVolatileLabelOverrideIsHonoured(t *testing.T) {
	r := parse(t, `
routes:
  - match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname]
    volatile_labels: [request_id]
`)
	got, _ := r.Match(core.Labels{"severity": "critical"})

	// pod is no longer volatile under this override, so it must now count.
	one := core.Labels{"alertname": "X", "pod": "a"}
	two := core.Labels{"alertname": "X", "pod": "b"}
	if got.Fingerprint(one) == got.Fingerprint(two) {
		t.Error("override ignored: pod should contribute when not listed as volatile")
	}

	three := core.Labels{"alertname": "X", "request_id": "r1"}
	four := core.Labels{"alertname": "X", "request_id": "r2"}
	if got.Fingerprint(three) != got.Fingerprint(four) {
		t.Error("override ignored: request_id should be excluded")
	}
}
