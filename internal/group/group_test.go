package group_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/route"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

const t0RFC = "2026-08-15T09:00:00Z"

const baseConfig = `
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
channels:
  ntfy:
    default_server: "https://ntfy.sh"
routes:
  - name: platform-critical
    match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname, cluster]
    group_wait: 30s
    group_interval: 5m
    resolve_grace: 2m
    volatile_labels: [pod, instance_id]
`

type harness struct {
	t     *testing.T
	db    *store.DB
	clk   *clock.FakeClock
	sched *timer.Scheduler
	eng   *group.Engine

	pagedMu sync.Mutex
	paged   []int64

	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T, cfgYAML string) *harness {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "group-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "kerberon.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg, err := config.Parse([]byte(cfgYAML), "test.yaml")
	if err != nil {
		t.Fatalf("config:\n%v", err)
	}

	h := &harness{t: t, db: db, clk: clock.NewFakeAt(t0RFC)}
	h.sched = timer.New(db, h.clk, timer.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.eng = group.New(db, h.clk, route.New(cfg), h.sched, group.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Stands in for the escalation engine, which arrives in Phase 5.
		// Counting calls is how these tests measure "how many pages".
		OnPageDue: func(ctx context.Context, tx *sql.Tx, inc core.Incident) error {
			h.pagedMu.Lock()
			h.paged = append(h.paged, inc.ID)
			h.pagedMu.Unlock()
			return nil
		},
	})
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		_ = h.sched.Run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-h.done
	})
}

func (h *harness) waitIdle() {
	h.t.Helper()
	h.clk.BlockUntil(1)
}

func (h *harness) pages() int {
	h.pagedMu.Lock()
	defer h.pagedMu.Unlock()
	return len(h.paged)
}

// alertFor builds a firing alert with the given labels.
func alertFor(now time.Time, labels core.Labels) core.Alert {
	return core.Alert{
		Source:      core.SourceAlertmanager,
		Status:      core.AlertFiring,
		Labels:      labels,
		Annotations: core.Annotations{},
		StartsAt:    now,
		ReceivedAt:  now,
	}
}

func (h *harness) ingest(alerts ...core.Alert) group.Result {
	h.t.Helper()
	res, err := h.eng.Ingest(context.Background(), alerts)
	if err != nil {
		h.t.Fatalf("ingest: %v", err)
	}
	return res
}

func (h *harness) openIncidents() []core.Incident {
	h.t.Helper()
	incs, err := h.db.Incidents(context.Background(),
		[]core.IncidentStatus{core.IncidentTriggered, core.IncidentAcknowledged}, 0)
	if err != nil {
		h.t.Fatalf("list incidents: %v", err)
	}
	return incs
}

// ─── Grouping ─────────────────────────────────────────────────────────────

func TestFirstAlertOpensAnIncident(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()

	res := h.ingest(alertFor(now, core.Labels{
		"alertname": "HighCPU", "cluster": "prod", "severity": "critical",
	}))

	if res.IncidentsCreated != 1 || res.AlertsAccepted != 1 {
		t.Fatalf("result = %+v, want one incident from one alert", res)
	}
	incs := h.openIncidents()
	if len(incs) != 1 {
		t.Fatalf("got %d open incidents, want 1", len(incs))
	}
	if incs[0].Status != core.IncidentTriggered {
		t.Errorf("status = %q, want triggered", incs[0].Status)
	}
	if incs[0].Team != "platform" {
		t.Errorf("team = %q, want platform", incs[0].Team)
	}
}

// The cascade claim the project is pitched on.
func TestCascadeOfManyAlertsProducesOneIncident(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()

	// 400 alerts from one bad deploy: same alertname and cluster, differing
	// only in the pod that happened to be affected.
	alerts := make([]core.Alert, 0, 400)
	for i := 0; i < 400; i++ {
		alerts = append(alerts, alertFor(now, core.Labels{
			"alertname": "HighCPU",
			"cluster":   "prod",
			"severity":  "critical",
			"pod":       fmt.Sprintf("api-%d", i),
		}))
	}

	res := h.ingest(alerts...)

	if res.IncidentsCreated != 1 {
		t.Fatalf("400 alerts created %d incidents, want 1", res.IncidentsCreated)
	}
	if res.AlertsAccepted != 400 {
		t.Errorf("accepted %d alerts, want 400", res.AlertsAccepted)
	}
	// pod is volatile, so all 400 share a fingerprint and 399 are duplicates.
	if res.AlertsDeduplicated != 399 {
		t.Errorf("deduplicated %d, want 399", res.AlertsDeduplicated)
	}

	// And crucially: one page, not four hundred.
	h.start()
	h.waitIdle()
	h.clk.Advance(30 * time.Second)
	h.waitIdle()

	if got := h.pages(); got != 1 {
		t.Fatalf("400 alerts produced %d pages, want 1", got)
	}
}

func TestDifferentGroupsProduceDifferentIncidents(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()

	res := h.ingest(
		alertFor(now, core.Labels{"alertname": "HighCPU", "cluster": "prod", "severity": "critical"}),
		alertFor(now, core.Labels{"alertname": "HighCPU", "cluster": "staging", "severity": "critical"}),
		alertFor(now, core.Labels{"alertname": "DiskFull", "cluster": "prod", "severity": "critical"}),
	)

	if res.IncidentsCreated != 3 {
		t.Fatalf("created %d incidents, want 3 distinct groups", res.IncidentsCreated)
	}
}

// A distinct alert joining an existing group is not a duplicate, and the counts
// must distinguish them or the dedup ratio in the UI is meaningless.
func TestRelatedAlertsJoinWithoutCountingAsDuplicates(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()

	h.ingest(alertFor(now, core.Labels{
		"alertname": "HighCPU", "cluster": "prod", "severity": "critical", "service": "api",
	}))
	res := h.ingest(alertFor(now, core.Labels{
		"alertname": "HighCPU", "cluster": "prod", "severity": "critical", "service": "web",
	}))

	if res.IncidentsCreated != 0 {
		t.Errorf("a related alert opened a new incident")
	}
	if res.AlertsDeduplicated != 0 {
		t.Errorf("a genuinely different alert was counted as a duplicate")
	}

	incs := h.openIncidents()
	if len(incs) != 1 {
		t.Fatalf("got %d open incidents, want 1", len(incs))
	}
	if incs[0].AlertCount != 2 {
		t.Errorf("alert_count = %d, want 2", incs[0].AlertCount)
	}
	if incs[0].DedupCount != 0 {
		t.Errorf("dedup_count = %d, want 0", incs[0].DedupCount)
	}
}

// An unrouted alert pages nobody. It must be counted, never silently dropped.
func TestUnroutedAlertsAreCountedNotDropped(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()

	res := h.ingest(alertFor(now, core.Labels{"alertname": "X", "severity": "info"}))

	if res.Unrouted != 1 {
		t.Errorf("unrouted = %d, want 1", res.Unrouted)
	}
	if res.IncidentsCreated != 0 {
		t.Errorf("an unrouted alert created an incident")
	}
}

// ─── group_wait ───────────────────────────────────────────────────────────

func TestNoPageBeforeGroupWaitElapses(t *testing.T) {
	h := newHarness(t, baseConfig)
	now := h.clk.Now()
	h.ingest(alertFor(now, core.Labels{"alertname": "HighCPU", "cluster": "prod", "severity": "critical"}))

	h.start()
	h.waitIdle()
	h.clk.Advance(29 * time.Second)
	h.waitIdle()

	if got := h.pages(); got != 0 {
		t.Fatalf("paged %d times before group_wait elapsed", got)
	}

	h.clk.Advance(time.Second)
	h.waitIdle()
	if got := h.pages(); got != 1 {
		t.Fatalf("paged %d times after group_wait, want 1", got)
	}
}

// ─── Resolution and flapping ──────────────────────────────────────────────

func TestIncidentResolvesAfterGraceWindow(t *testing.T) {
	h := newHarness(t, baseConfig)
	labels := core.Labels{"alertname": "HighCPU", "cluster": "prod", "severity": "critical"}

	h.ingest(alertFor(h.clk.Now(), labels))
	h.start()
	h.waitIdle()
	h.clk.Advance(30 * time.Second)
	h.waitIdle()

	// The alert resolves.
	resolved := alertFor(h.clk.Now(), labels)
	resolved.Status = core.AlertResolved
	end := h.clk.Now()
	resolved.EndsAt = &end
	h.ingest(resolved)

	h.waitIdle()
	if got := len(h.openIncidents()); got != 1 {
		t.Fatalf("incident closed before its grace window elapsed (%d open)", got)
	}

	h.clk.Advance(2 * time.Minute)
	h.waitIdle()

	if got := len(h.openIncidents()); got != 0 {
		t.Fatalf("%d incidents still open after the grace window", got)
	}
}

// The flapping guard: an alert oscillating must not produce a
// page-resolve-page storm.
func TestReFiringWithinGraceKeepsTheIncidentOpen(t *testing.T) {
	h := newHarness(t, baseConfig)
	labels := core.Labels{"alertname": "Flappy", "cluster": "prod", "severity": "critical"}

	h.ingest(alertFor(h.clk.Now(), labels))
	h.start()
	h.waitIdle()
	h.clk.Advance(30 * time.Second)
	h.waitIdle()

	if got := h.pages(); got != 1 {
		t.Fatalf("pages = %d, want 1 after the first group_wait", got)
	}

	// Oscillate several times, each cycle shorter than resolve_grace.
	for i := 0; i < 5; i++ {
		resolved := alertFor(h.clk.Now(), labels)
		resolved.Status = core.AlertResolved
		end := h.clk.Now()
		resolved.EndsAt = &end
		h.ingest(resolved)

		h.waitIdle()
		h.clk.Advance(30 * time.Second)
		h.waitIdle()

		h.ingest(alertFor(h.clk.Now(), labels))
		h.waitIdle()
		h.clk.Advance(30 * time.Second)
		h.waitIdle()
	}

	if got := len(h.openIncidents()); got != 1 {
		t.Errorf("flapping alert left %d open incidents, want the original 1", got)
	}
	// The whole point: no new pages from the oscillation.
	if got := h.pages(); got != 1 {
		t.Errorf("flapping produced %d pages, want 1", got)
	}
}

// A resolved alert for a group with no open incident must not open one, or a
// resolution would page someone about a problem that is already over.
func TestResolvedAlertForUnknownGroupDoesNotPage(t *testing.T) {
	h := newHarness(t, baseConfig)
	labels := core.Labels{"alertname": "Ghost", "cluster": "prod", "severity": "critical"}

	a := alertFor(h.clk.Now(), labels)
	a.Status = core.AlertResolved
	end := h.clk.Now()
	a.EndsAt = &end

	res := h.ingest(a)
	if res.IncidentsCreated != 0 {
		t.Errorf("a resolved alert opened %d incidents", res.IncidentsCreated)
	}
	if got := len(h.openIncidents()); got != 0 {
		t.Errorf("%d incidents open, want 0", got)
	}
}

// An incident closes only when every distinct alert in it has resolved.
func TestPartialResolutionKeepsTheIncidentOpen(t *testing.T) {
	h := newHarness(t, baseConfig)
	base := core.Labels{"alertname": "HighCPU", "cluster": "prod", "severity": "critical"}

	api := core.Labels{}
	web := core.Labels{}
	for k, v := range base {
		api[k], web[k] = v, v
	}
	api["service"] = "api"
	web["service"] = "web"

	h.ingest(alertFor(h.clk.Now(), api), alertFor(h.clk.Now(), web))
	h.start()
	h.waitIdle()
	h.clk.Advance(30 * time.Second)
	h.waitIdle()

	// Only one of the two resolves.
	resolved := alertFor(h.clk.Now(), api)
	resolved.Status = core.AlertResolved
	end := h.clk.Now()
	resolved.EndsAt = &end
	h.ingest(resolved)

	h.waitIdle()
	h.clk.Advance(5 * time.Minute)
	h.waitIdle()

	if got := len(h.openIncidents()); got != 1 {
		t.Errorf("incident closed while one of its alerts was still firing (%d open)", got)
	}
}

// After closing, a fresh occurrence must be able to open a new incident.
func TestGroupKeyIsReusableAfterResolution(t *testing.T) {
	h := newHarness(t, baseConfig)
	labels := core.Labels{"alertname": "HighCPU", "cluster": "prod", "severity": "critical"}

	h.ingest(alertFor(h.clk.Now(), labels))
	h.start()
	h.waitIdle()
	h.clk.Advance(30 * time.Second)
	h.waitIdle()

	resolved := alertFor(h.clk.Now(), labels)
	resolved.Status = core.AlertResolved
	end := h.clk.Now()
	resolved.EndsAt = &end
	h.ingest(resolved)
	h.waitIdle()
	h.clk.Advance(2 * time.Minute)
	h.waitIdle()

	if got := len(h.openIncidents()); got != 0 {
		t.Fatalf("%d incidents still open", got)
	}

	res := h.ingest(alertFor(h.clk.Now(), labels))
	if res.IncidentsCreated != 1 {
		t.Errorf("a recurrence created %d incidents, want 1", res.IncidentsCreated)
	}
}

// ─── Timeline ─────────────────────────────────────────────────────────────

func TestIncidentTimelineRecordsCreation(t *testing.T) {
	h := newHarness(t, baseConfig)
	h.ingest(alertFor(h.clk.Now(), core.Labels{
		"alertname": "HighCPU", "cluster": "prod", "severity": "critical",
	}))

	incs := h.openIncidents()
	events, err := h.db.Events(context.Background(), incs[0].ID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no timeline events recorded")
	}
	if events[0].Kind != core.EventCreated {
		t.Errorf("first event = %q, want created", events[0].Kind)
	}
}
