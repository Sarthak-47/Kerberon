// Package ingest exposes the HTTP endpoints monitoring systems post alerts to.
//
// Every handler does the same three things: authenticate, normalize the payload
// into Kerberon's internal Alert, and hand the result to the grouping engine.
// Adding a source means writing one normalizer in internal/alert and one thin
// handler here (spec section 6.1).
package ingest

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/alert"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
)

// DefaultMaxBodyBytes caps a request body. Alertmanager batches a whole group
// into one webhook so legitimate payloads can be large, but an unbounded read
// is a trivial way to exhaust memory on a service whose entire job is to stay
// up when everything else is failing.
const DefaultMaxBodyBytes = 8 << 20 // 8 MiB

// Ingester is the part of the grouping engine this package needs. Narrowing it
// keeps the handlers testable without a database.
type Ingester interface {
	Ingest(ctx context.Context, alerts []core.Alert) (group.Result, error)
}

// Options configures a Server.
type Options struct {
	// Token authenticates every ingest request. Required.
	Token string
	// MaxBodyBytes defaults to DefaultMaxBodyBytes.
	MaxBodyBytes int64
	Logger       *slog.Logger
}

// Server holds the ingest handlers.
type Server struct {
	engine  Ingester
	clk     clock.Clock
	token   []byte
	maxBody int64
	log     *slog.Logger
}

// New builds a Server. token must be non-empty; an ingest endpoint without
// authentication is open to anyone who can reach the port.
func New(engine Ingester, clk clock.Clock, opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("ingest: a token is required")
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		engine:  engine,
		clk:     clk,
		token:   []byte(opts.Token),
		maxBody: opts.MaxBodyBytes,
		log:     log,
	}, nil
}

// Routes mounts the ingest API.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	// Unauthenticated: liveness only, exposing nothing.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.Authenticate)
		r.Post("/alerts", s.HandleGeneric)
		r.Post("/alertmanager", s.HandleAlertmanager)
		r.Post("/grafana", s.HandleGrafana)
	})

	return r
}

// ─── Authentication ───────────────────────────────────────────────────────

// Authenticate checks the bearer token in constant time. It is exported so
// internal/api can apply the same check to the query endpoints it adds.
//
// The comparison is constant time deliberately: a variable-time
// comparison leaks the token one byte at a time to anyone who can measure
// response latency, and this token authorises raising incidents.
func (s *Server) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(presented), s.token) != 1 {
			s.log.Warn("rejected ingest request with a bad or missing token",
				"path", r.URL.Path, "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="kerberon"`)
			writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	// Some webhook senders cannot set headers. A query parameter is accepted
	// as a fallback, though it is weaker: URLs end up in proxy and server logs.
	return r.URL.Query().Get("token")
}

// ─── Handlers ─────────────────────────────────────────────────────────────

type parser func(body []byte, receivedAt time.Time) ([]core.Alert, error)

func (s *Server) HandleGeneric(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, "generic", alert.ParseGeneric)
}

func (s *Server) HandleAlertmanager(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, "alertmanager", alert.ParseAlertmanager)
}

func (s *Server) HandleGrafana(w http.ResponseWriter, r *http.Request) {
	s.handle(w, r, "grafana", alert.ParseGrafana)
}

// response is what an ingest endpoint returns. The counts let an operator
// verify grouping is doing what they expect without opening the UI.
type response struct {
	Accepted     int `json:"accepted"`
	IncidentsNew int `json:"incidents_created"`
	Deduplicated int `json:"deduplicated"`
	Unrouted     int `json:"unrouted"`
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request, source string, parse parser) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("payload exceeds the %d byte limit", s.maxBody))
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	alerts, err := parse(body, s.clk.Now())
	if err != nil {
		// A malformed payload is the sender's bug, but it also means alerts
		// are being lost, so it is logged rather than only returned.
		s.log.Warn("rejected malformed payload", "source", source, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.engine.Ingest(r.Context(), alerts)
	if err != nil {
		s.log.Error("ingest failed", "source", source, "error", err)
		writeError(w, http.StatusInternalServerError, "could not store alerts")
		return
	}

	if res.Unrouted > 0 {
		// Already logged per alert by the engine; repeated here with a count
		// because "nothing happened" is otherwise invisible to the sender.
		s.log.Warn("some alerts matched no route and will never page",
			"source", source, "unrouted", res.Unrouted, "accepted", res.AlertsAccepted)
	}

	writeJSON(w, http.StatusOK, response{
		Accepted:     res.AlertsAccepted,
		IncidentsNew: res.IncidentsCreated,
		Deduplicated: res.AlertsDeduplicated,
		Unrouted:     res.Unrouted,
	})
}

// ─── Responses ────────────────────────────────────────────────────────────

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
