package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/api"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/ingest"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

const token = "test-token"

type nopEngine struct{}

func (nopEngine) Ingest(context.Context, []core.Alert) (group.Result, error) {
	return group.Result{}, nil
}

// fixedOverrides stands in for the store.
type fixedOverrides struct{ items []core.Override }

func (f fixedOverrides) OverridesInWindow(_ context.Context, name string, from, to time.Time) ([]core.Override, error) {
	var out []core.Override
	for _, o := range f.items {
		if o.ScheduleName == name && o.StartsAt.Before(to) && o.EndsAt.After(from) {
			out = append(out, o)
		}
	}
	return out, nil
}

const cfgYAML = `
server:
  external_url: "https://k.example.com"
  secret_key: "s"
  ingest_token: "test-token"
users:
  - id: sarthak
    name: Sarthak
    timezone: "Asia/Kolkata"
    contacts: {ntfy: "https://ntfy.sh/a"}
  - id: priya
    name: Priya
    timezone: "Asia/Kolkata"
    contacts: {ntfy: "https://ntfy.sh/b"}
teams:
  - name: platform
    members: [sarthak, priya]
  - name: data
    members: [priya]
schedules:
  - name: platform-primary
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: base
        type: rotation
        participants: [sarthak, priya]
        rotation: weekly
        handoff: {day: monday, time: "09:00"}
  - name: data-primary
    team: data
    timezone: "Asia/Kolkata"
    layers:
      - name: business-hours
        type: restriction
        participants: [priya]
        restriction:
          days: [monday, tuesday, wednesday, thursday, friday]
          start: "09:00"
          end: "18:00"
escalation_policies:
  - name: p
    steps:
      - delay: 0
        targets: [team:platform]
        channels: [ntfy]
routes:
  - match: {severity: critical}
    team: platform
    policy: p
    group_by: [alertname]
channels:
  ntfy:
    default_server: "https://ntfy.sh"
`

func newServer(t *testing.T, overrides api.OverrideReader) http.Handler {
	t.Helper()

	cfg, err := config.Parse([]byte(cfgYAML), "test.yaml")
	if err != nil {
		t.Fatalf("config:\n%v", err)
	}
	schedules, err := schedule.FromConfig(cfg)
	if err != nil {
		t.Fatalf("schedules: %v", err)
	}
	clk := clock.NewFakeAt("2026-08-19T12:00:00Z")

	ing, err := ingest.New(nopEngine{}, clk, ingest.Options{
		Token:  token,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	return api.New(ing, schedules, clk, api.Options{
		Overrides: overrides,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Routes()
}

func get(t *testing.T, h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type oncallBody struct {
	At     string `json:"at"`
	OnCall []struct {
		Schedule string `json:"schedule"`
		Team     string `json:"team"`
		User     string `json:"user"`
		Covered  bool   `json:"covered"`
	} `json:"oncall"`
	Schedule  string `json:"schedule"`
	Intervals []struct {
		Start string `json:"start"`
		End   string `json:"end"`
		User  string `json:"user"`
		Gap   bool   `json:"gap"`
	} `json:"intervals"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) oncallBody {
	t.Helper()
	var b oncallBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return b
}

// The on-call endpoint carries the same authentication as ingest; it discloses
// who is reachable at 3am and should not be public.
func TestOnCallRequiresAuthentication(t *testing.T) {
	h := newServer(t, nil)
	if rec := get(t, h, "/api/v1/oncall", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec := get(t, h, "/api/v1/oncall", "wrong"); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d with a bad token, want 401", rec.Code)
	}
}

func TestOnCallReturnsEverySchedule(t *testing.T) {
	rec := get(t, newServer(t, nil), "/api/v1/oncall?at=2026-08-19T12:00:00Z", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if len(body.OnCall) != 2 {
		t.Fatalf("got %d schedules, want 2", len(body.OnCall))
	}
	for _, e := range body.OnCall {
		if e.Covered && e.User == "" {
			t.Errorf("schedule %q is covered but names nobody", e.Schedule)
		}
	}
}

func TestOnCallFiltersByTeam(t *testing.T) {
	rec := get(t, newServer(t, nil), "/api/v1/oncall?team=data&at=2026-08-19T12:00:00Z", token)
	body := decode(t, rec)
	if len(body.OnCall) != 1 || body.OnCall[0].Team != "data" {
		t.Fatalf("team filter returned %+v", body.OnCall)
	}
}

// "Nobody is on call" is a real answer, and the caller must be able to tell it
// apart from "that schedule does not exist".
func TestUncoveredIsAnAnswerNotAnError(t *testing.T) {
	// 03:00 IST on a weekday is outside the data team's business-hours window.
	rec := get(t, newServer(t, nil),
		"/api/v1/oncall?schedule=data-primary&at=2026-08-19T21:30:00Z", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if len(body.OnCall) != 1 {
		t.Fatalf("got %d entries", len(body.OnCall))
	}
	if body.OnCall[0].Covered {
		t.Error("outside business hours should report covered=false")
	}

	// A schedule that does not exist is a 404, which is a different thing.
	if rec := get(t, newServer(t, nil), "/api/v1/oncall?schedule=nope", token); rec.Code != http.StatusNotFound {
		t.Errorf("unknown schedule status = %d, want 404", rec.Code)
	}
}

func TestRotaMarksGaps(t *testing.T) {
	rec := get(t, newServer(t, nil),
		"/api/v1/oncall?schedule=data-primary&at=2026-08-17T03:30:00Z&days=7", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	body := decode(t, rec)

	var gaps, covered int
	for _, iv := range body.Intervals {
		if iv.Gap {
			gaps++
			if iv.User != "" {
				t.Errorf("a gap names a user: %+v", iv)
			}
		} else {
			covered++
		}
	}
	if gaps == 0 {
		t.Error("a business-hours schedule should report gaps in its rota")
	}
	if covered == 0 {
		t.Error("a business-hours schedule should report covered spans too")
	}

	// The rota must be ordered, or a calendar renders nonsense.
	for i := 1; i < len(body.Intervals); i++ {
		if body.Intervals[i].Start < body.Intervals[i-1].Start {
			t.Fatalf("intervals are out of order at %d", i)
		}
	}
}

func TestOverridesAreApplied(t *testing.T) {
	const query = "/api/v1/oncall?schedule=platform-primary&at=2026-08-19T12:00:00Z"

	without := decode(t, get(t, newServer(t, nil), query, token))
	base := without.OnCall[0].User
	if base == "" {
		t.Fatal("nobody is on call without an override; the test cannot distinguish anything")
	}

	// Cover with whoever is *not* already on call, so a passing result cannot
	// be a coincidence of where the rotation happens to be that week.
	cover := "priya"
	if base == "priya" {
		cover = "sarthak"
	}

	ov := core.Override{
		ScheduleName: "platform-primary",
		UserID:       cover,
		StartsAt:     time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC),
		EndsAt:       time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC),
		CreatedAt:    time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC),
	}
	with := decode(t, get(t, newServer(t, fixedOverrides{items: []core.Override{ov}}), query, token))

	if with.OnCall[0].User != cover {
		t.Errorf("override not applied: got %q, want %q (base rotation was %q)",
			with.OnCall[0].User, cover, base)
	}
}

func TestBadParametersAreRejected(t *testing.T) {
	h := newServer(t, nil)
	cases := []struct{ name, path string }{
		{"bad at", "/api/v1/oncall?at=yesterday"},
		{"bad days", "/api/v1/oncall?schedule=platform-primary&days=abc"},
		{"days too large", "/api/v1/oncall?schedule=platform-primary&days=4000"},
		{"days without a schedule", "/api/v1/oncall?days=7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(t, h, c.path, token)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}

// Ingest still works through the composed router.
func TestIngestRoutesAreStillMounted(t *testing.T) {
	h := newServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// An empty body is a bad request, not a missing route.
	if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
		t.Errorf("ingest route is not mounted: status %d", rec.Code)
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	if rec := get(t, newServer(t, nil), "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
