package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/ingest"
)

const (
	token = "test-ingest-token"
	t0RFC = "2026-08-15T09:00:00Z"
)

// fakeEngine records what was ingested without touching a database.
type fakeEngine struct {
	got    [][]core.Alert
	result group.Result
	err    error
}

func (f *fakeEngine) Ingest(_ context.Context, alerts []core.Alert) (group.Result, error) {
	f.got = append(f.got, alerts)
	if f.err != nil {
		return group.Result{}, f.err
	}
	res := f.result
	if res == (group.Result{}) {
		res = group.Result{AlertsAccepted: len(alerts)}
	}
	return res, nil
}

func newServer(t *testing.T, eng ingest.Ingester) http.Handler {
	t.Helper()
	s, err := ingest.New(eng, clock.NewFakeAt(t0RFC), ingest.Options{
		Token:  token,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.Routes()
}

func post(t *testing.T, h http.Handler, path, body, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const genericBody = `{"labels":{"alertname":"DiskFull","severity":"critical"}}`

// ─── Authentication ───────────────────────────────────────────────────────

// These endpoints can raise incidents, so an unauthenticated request must not
// reach the engine at all.
func TestIngestRequiresAValidToken(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{"no token", ""},
		{"wrong token", "not-the-token"},
		{"prefix of the real token", token[:5]},
		{"token with trailing data", token + "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng := &fakeEngine{}
			rec := post(t, newServer(t, eng), "/api/v1/alerts", genericBody, c.bearer)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if len(eng.got) != 0 {
				t.Error("an unauthenticated request reached the engine")
			}
			if got := rec.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("401 should carry a WWW-Authenticate header")
			}
		})
	}
}

func TestValidTokenIsAccepted(t *testing.T) {
	eng := &fakeEngine{}
	rec := post(t, newServer(t, eng), "/api/v1/alerts", genericBody, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	if len(eng.got) != 1 {
		t.Fatalf("engine called %d times, want 1", len(eng.got))
	}
}

// Some webhook senders cannot set headers.
func TestTokenMayBeSuppliedAsAQueryParameter(t *testing.T) {
	eng := &fakeEngine{}
	rec := post(t, newServer(t, eng), "/api/v1/alerts?token="+token, genericBody, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestServerRequiresATokenToBeConfigured(t *testing.T) {
	_, err := ingest.New(&fakeEngine{}, clock.NewFakeAt(t0RFC), ingest.Options{})
	if err == nil {
		t.Fatal("a server with no token should be refused; the endpoints would be open to anyone")
	}
}

// Health is deliberately unauthenticated so a load balancer can reach it, and
// must therefore reveal nothing.
func TestHealthzNeedsNoToken(t *testing.T) {
	h := newServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Error("healthz leaked the ingest token")
	}
}

// ─── Payload handling ─────────────────────────────────────────────────────

func TestGenericEndpointNormalizesAndForwards(t *testing.T) {
	eng := &fakeEngine{}
	rec := post(t, newServer(t, eng), "/api/v1/alerts", genericBody, token)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	alerts := eng.got[0]
	if len(alerts) != 1 {
		t.Fatalf("forwarded %d alerts, want 1", len(alerts))
	}
	if alerts[0].Source != core.SourceGeneric {
		t.Errorf("source = %q, want generic", alerts[0].Source)
	}
	if alerts[0].Labels["alertname"] != "DiskFull" {
		t.Errorf("labels not preserved: %v", alerts[0].Labels)
	}
	// The receive time comes from the injected clock, not the wall clock.
	if got := alerts[0].ReceivedAt.Format("2006-01-02T15:04:05Z"); got != t0RFC {
		t.Errorf("receivedAt = %s, want the fake clock's %s", got, t0RFC)
	}
}

const alertmanagerBody = `{
  "version": "4",
  "status": "firing",
  "commonLabels": {"alertname": "HighCPU"},
  "alerts": [
    {"status":"firing","labels":{"service":"api","severity":"critical"},
     "annotations":{"summary":"api down"},
     "startsAt":"2026-08-15T09:00:00Z","endsAt":"0001-01-01T00:00:00Z"}
  ]
}`

func TestAlertmanagerAndGrafanaEndpointsRecordTheirSource(t *testing.T) {
	for _, c := range []struct {
		path string
		want core.Source
	}{
		{"/api/v1/alertmanager", core.SourceAlertmanager},
		{"/api/v1/grafana", core.SourceGrafana},
	} {
		t.Run(string(c.want), func(t *testing.T) {
			eng := &fakeEngine{}
			rec := post(t, newServer(t, eng), c.path, alertmanagerBody, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			if got := eng.got[0][0].Source; got != c.want {
				t.Errorf("source = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMalformedPayloadIsRejected(t *testing.T) {
	cases := []struct{ name, path, body string }{
		{"not json", "/api/v1/alerts", `{{{`},
		{"no labels", "/api/v1/alerts", `{"annotations":{"summary":"x"}}`},
		{"no alerts", "/api/v1/alertmanager", `{"version":"4","alerts":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng := &fakeEngine{}
			rec := post(t, newServer(t, eng), c.path, c.body, token)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if len(eng.got) != 0 {
				t.Error("a malformed payload reached the engine")
			}
			// The sender needs to know what was wrong with it.
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not JSON: %s", rec.Body)
			}
			if body["error"] == "" {
				t.Error("error response carried no explanation")
			}
		})
	}
}

// An unbounded read is a trivial way to exhaust memory on a service whose whole
// job is to stay up when everything else is failing.
func TestOversizedBodyIsRejected(t *testing.T) {
	eng := &fakeEngine{}
	s, err := ingest.New(eng, clock.NewFakeAt(t0RFC), ingest.Options{
		Token:        token,
		MaxBodyBytes: 512,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	big := `{"labels":{"alertname":"` + strings.Repeat("x", 2000) + `"}}`
	rec := post(t, s.Routes(), "/api/v1/alerts", big, token)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if len(eng.got) != 0 {
		t.Error("an oversized payload reached the engine")
	}
}

// ─── Responses ────────────────────────────────────────────────────────────

// The counts let an operator confirm grouping is behaving without opening the
// UI — the cascade demo is checkable straight from curl.
func TestResponseReportsGroupingCounts(t *testing.T) {
	eng := &fakeEngine{result: group.Result{
		AlertsAccepted:     400,
		IncidentsCreated:   1,
		AlertsDeduplicated: 399,
		Unrouted:           2,
	}}
	rec := post(t, newServer(t, eng), "/api/v1/alerts", genericBody, token)

	var body struct {
		Accepted     int `json:"accepted"`
		IncidentsNew int `json:"incidents_created"`
		Deduplicated int `json:"deduplicated"`
		Unrouted     int `json:"unrouted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if body.Accepted != 400 || body.IncidentsNew != 1 || body.Deduplicated != 399 || body.Unrouted != 2 {
		t.Errorf("counts = %+v, want the engine's result", body)
	}
}

func TestEngineFailureIsAServerError(t *testing.T) {
	eng := &fakeEngine{err: errStore}
	rec := post(t, newServer(t, eng), "/api/v1/alerts", genericBody, token)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	// The client must not be told a storage failure was their fault, nor be
	// given internal detail.
	if strings.Contains(rec.Body.String(), errStore.Error()) {
		t.Error("internal error detail leaked to the client")
	}
}

var errStore = errStoreType{}

type errStoreType struct{}

func (errStoreType) Error() string { return "disk on fire: internal detail" }

func TestGetIsNotAllowedOnIngestEndpoints(t *testing.T) {
	h := newServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/alerts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
