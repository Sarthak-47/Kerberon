package heartbeat_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/heartbeat"
	"github.com/Sarthak-47/kerberon/internal/store"
)

const t0RFC = "2026-08-15T09:00:00Z"

// sink records the synthetic alerts a missed heartbeat produces.
type sink struct {
	mu   sync.Mutex
	got  [][]core.Alert
	fail error
}

func (s *sink) Ingest(_ context.Context, alerts []core.Alert) (group.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return group.Result{}, s.fail
	}
	s.got = append(s.got, alerts)
	return group.Result{AlertsAccepted: len(alerts)}, nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *sink) last() core.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[len(s.got)-1][0]
}

type harness struct {
	t    *testing.T
	db   *store.DB
	clk  *clock.FakeClock
	sink *sink
	swp  *heartbeat.Sweeper
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "heartbeat-test-")
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

	h := &harness{t: t, db: db, clk: clock.NewFakeAt(t0RFC), sink: &sink{}}
	h.swp = heartbeat.New(db, h.clk, h.sink, heartbeat.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h
}

// register adds a heartbeat and returns its token.
func (h *harness) register(name string, interval, grace time.Duration) string {
	h.t.Helper()
	created, err := heartbeat.Sync(context.Background(), h.db, []heartbeat.DeclaredHeartbeat{{
		Name:             name,
		ExpectedInterval: interval,
		GracePeriod:      grace,
		Team:             "platform",
		Severity:         core.SeverityCritical,
	}}, h.clk.Now())
	if err != nil {
		h.t.Fatalf("sync: %v", err)
	}
	token, ok := created[name]
	if !ok {
		h.t.Fatalf("no token minted for %q", name)
	}
	return token
}

func (h *harness) ping(token string) {
	h.t.Helper()
	if _, err := h.db.RecordPing(context.Background(), token, h.clk.Now()); err != nil {
		h.t.Fatalf("ping: %v", err)
	}
}

func (h *harness) state(name string) core.HeartbeatState {
	h.t.Helper()
	all, err := h.db.Heartbeats(context.Background())
	if err != nil {
		h.t.Fatalf("list: %v", err)
	}
	for _, x := range all {
		if x.Name == name {
			return x.State
		}
	}
	h.t.Fatalf("no heartbeat named %q", name)
	return ""
}

func (h *harness) sweep() int {
	h.t.Helper()
	n, err := h.swp.SweepOnce(context.Background())
	if err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
	return n
}

// ─── Registration ─────────────────────────────────────────────────────────

// The token lets its holder keep a dead job looking alive, so it must be
// unguessable and must not come from configuration.
func TestTokensAreMintedAndUnguessable(t *testing.T) {
	h := newHarness(t)

	a := h.register("nightly-backup", time.Hour, 10*time.Minute)
	b := h.register("hourly-sync", time.Hour, 10*time.Minute)

	if a == b {
		t.Fatal("two heartbeats got the same token")
	}
	if len(a) < 24 {
		t.Errorf("token is only %d characters; too short to resist guessing", len(a))
	}
}

// Sync runs on every startup, so a heartbeat already registered must not get a
// second row or a new token.
func TestSyncIsIdempotent(t *testing.T) {
	h := newHarness(t)
	declared := []heartbeat.DeclaredHeartbeat{{
		Name: "nightly-backup", ExpectedInterval: time.Hour,
		GracePeriod: time.Minute, Team: "platform", Severity: core.SeverityCritical,
	}}
	ctx := context.Background()

	first, err := heartbeat.Sync(ctx, h.db, declared, h.clk.Now())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first sync created %d heartbeats, want 1", len(first))
	}

	second, err := heartbeat.Sync(ctx, h.db, declared, h.clk.Now())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second sync minted %d new tokens; an existing heartbeat must keep its own", len(second))
	}

	all, _ := h.db.Heartbeats(ctx)
	if len(all) != 1 {
		t.Errorf("got %d heartbeat rows, want 1", len(all))
	}
}

// ─── The switch ───────────────────────────────────────────────────────────

// A heartbeat that has never been pinged is not late; it has not started.
// Paging about it would mean paging about a job that was never deployed.
func TestNeverPingedIsNotOverdue(t *testing.T) {
	h := newHarness(t)
	h.register("never-run", time.Minute, time.Minute)

	h.clk.Advance(24 * time.Hour)
	if n := h.sweep(); n != 0 {
		t.Errorf("swept %d never-pinged heartbeats as missing", n)
	}
	if got := h.state("never-run"); got != core.HeartbeatNeverSeen {
		t.Errorf("state = %q, want never_seen", got)
	}
	if h.sink.count() != 0 {
		t.Error("a never-pinged heartbeat raised an alert")
	}
}

func TestHealthyHeartbeatIsNotSwept(t *testing.T) {
	h := newHarness(t)
	token := h.register("hourly-sync", time.Hour, 5*time.Minute)
	h.ping(token)

	// Inside interval + grace.
	h.clk.Advance(time.Hour + 4*time.Minute)
	if n := h.sweep(); n != 0 {
		t.Errorf("swept %d heartbeats that were still within their window", n)
	}
	if got := h.state("hourly-sync"); got != core.HeartbeatHealthy {
		t.Errorf("state = %q, want healthy", got)
	}
}

// The whole point: a cron that silently stopped produces an incident, with no
// metric anywhere to have alerted on.
func TestMissedHeartbeatRaisesAnAlert(t *testing.T) {
	h := newHarness(t)
	token := h.register("nightly-backup", time.Hour, 5*time.Minute)
	h.ping(token)

	h.clk.Advance(time.Hour + 6*time.Minute)
	if n := h.sweep(); n != 1 {
		t.Fatalf("swept %d heartbeats, want 1", n)
	}
	if got := h.state("nightly-backup"); got != core.HeartbeatMissing {
		t.Errorf("state = %q, want missing", got)
	}
	if h.sink.count() != 1 {
		t.Fatalf("raised %d alerts, want 1", h.sink.count())
	}

	a := h.sink.last()
	if a.Source != core.SourceHeartbeat {
		t.Errorf("source = %q, want heartbeat", a.Source)
	}
	if a.Status != core.AlertFiring {
		t.Errorf("status = %q, want firing", a.Status)
	}
	// The labels must let an operator route a heartbeat like any other alert.
	for k, want := range map[string]string{
		"alertname": "HeartbeatMissing",
		"heartbeat": "nightly-backup",
		"team":      "platform",
		"severity":  "critical",
	} {
		if a.Labels[k] != want {
			t.Errorf("label %q = %q, want %q", k, a.Labels[k], want)
		}
	}
	if a.Annotations["summary"] == "" {
		t.Error("no summary; the page would have no title")
	}
}

// A switch that stays down must produce one incident, not one per sweep.
func TestAMissingHeartbeatRaisesOnlyOnce(t *testing.T) {
	h := newHarness(t)
	token := h.register("nightly-backup", time.Minute, 0)
	h.ping(token)

	h.clk.Advance(2 * time.Minute)
	if n := h.sweep(); n != 1 {
		t.Fatalf("first sweep raised %d, want 1", n)
	}

	for i := 0; i < 5; i++ {
		h.clk.Advance(time.Minute)
		if n := h.sweep(); n != 0 {
			t.Fatalf("sweep %d raised %d more alerts for a switch already known missing", i, n)
		}
	}
	if h.sink.count() != 1 {
		t.Errorf("raised %d alerts in total, want 1", h.sink.count())
	}
}

// A job coming back must return to healthy, and be able to raise again if it
// stops a second time.
func TestRecoveryAndSecondFailure(t *testing.T) {
	h := newHarness(t)
	token := h.register("hourly-sync", time.Minute, 0)
	h.ping(token)

	h.clk.Advance(2 * time.Minute)
	h.sweep()
	if got := h.state("hourly-sync"); got != core.HeartbeatMissing {
		t.Fatalf("state = %q, want missing", got)
	}

	// It comes back.
	h.ping(token)
	if got := h.state("hourly-sync"); got != core.HeartbeatHealthy {
		t.Fatalf("state = %q after a ping, want healthy", got)
	}

	// And stops again.
	h.clk.Advance(2 * time.Minute)
	if n := h.sweep(); n != 1 {
		t.Fatalf("second failure raised %d, want 1", n)
	}
	if h.sink.count() != 2 {
		t.Errorf("raised %d alerts across two failures, want 2", h.sink.count())
	}
}

// RecordPing reports the prior state so a caller can tell a recovery from an
// ordinary ping.
func TestPingReportsThePreviousState(t *testing.T) {
	h := newHarness(t)
	token := h.register("hourly-sync", time.Minute, 0)
	ctx := context.Background()

	before, err := h.db.RecordPing(ctx, token, h.clk.Now())
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if before.State != core.HeartbeatNeverSeen {
		t.Errorf("first ping reported prior state %q, want never_seen", before.State)
	}

	h.clk.Advance(2 * time.Minute)
	h.sweep()

	before, err = h.db.RecordPing(ctx, token, h.clk.Now())
	if err != nil {
		t.Fatalf("recovery ping: %v", err)
	}
	if before.State != core.HeartbeatMissing {
		t.Errorf("recovery ping reported prior state %q, want missing", before.State)
	}
}

func TestUnknownTokenIsRejected(t *testing.T) {
	h := newHarness(t)
	h.register("hourly-sync", time.Minute, 0)

	if _, err := h.db.RecordPing(context.Background(), "not-a-real-token", h.clk.Now()); err == nil {
		t.Fatal("an unknown token was accepted")
	}
}

// A grace period is what stops a job that is merely slow from paging someone.
func TestGracePeriodIsHonoured(t *testing.T) {
	h := newHarness(t)
	token := h.register("slow-job", time.Minute, 10*time.Minute)
	h.ping(token)

	h.clk.Advance(10 * time.Minute)
	if n := h.sweep(); n != 0 {
		t.Errorf("swept a job that was late but still inside its grace period")
	}

	h.clk.Advance(2 * time.Minute)
	if n := h.sweep(); n != 1 {
		t.Errorf("did not sweep a job past interval plus grace")
	}
}
