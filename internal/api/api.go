// Package api owns Kerberon's HTTP route tree.
//
// It composes the ingest handlers with the read endpoints rather than owning
// either, so ingest stays about normalizing payloads and this package stays
// about routing and shaping responses.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/ack"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/ingest"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

// maxOnCallDays bounds a rota request. Rendering a decade of intervals is a
// cheap way to make the process do a lot of work on someone else's behalf.
const maxOnCallDays = 400

// OverrideReader is the slice of the store the on-call endpoint needs.
type OverrideReader interface {
	OverridesInWindow(ctx context.Context, scheduleName string, from, to time.Time) ([]core.Override, error)
}

// Server routes the HTTP API.
type Server struct {
	ingest    *ingest.Server
	schedules map[string]*schedule.Schedule
	overrides OverrideReader
	clk       clock.Clock
	log       *slog.Logger

	// signer, acker and incidents are optional: a server built without them
	// still serves ingest and on-call, and the acknowledgement endpoints say
	// so plainly rather than failing obscurely.
	signer    *ack.Signer
	acker     Acknowledger
	incidents IncidentStore
}

// Options configures a Server.
type Options struct {
	// Overrides may be nil, in which case rotations resolve without them.
	Overrides OverrideReader
	// Signer, Acknowledger and Incidents enable the acknowledgement
	// endpoints. Omit them and those routes report that they are unconfigured.
	Signer       *ack.Signer
	Acknowledger Acknowledger
	Incidents    IncidentStore
	Logger       *slog.Logger
}

// New composes the route tree.
func New(ing *ingest.Server, schedules map[string]*schedule.Schedule, clk clock.Clock, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		ingest:    ing,
		schedules: schedules,
		overrides: opts.Overrides,
		clk:       clk,
		log:       log,
		signer:    opts.Signer,
		acker:     opts.Acknowledger,
		incidents: opts.Incidents,
	}
}

// Routes returns the handler to serve.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	// Unauthenticated: liveness only, revealing nothing.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.ingest.Authenticate)

		r.Post("/alerts", s.ingest.HandleGeneric)
		r.Post("/alertmanager", s.ingest.HandleAlertmanager)
		r.Post("/grafana", s.ingest.HandleGrafana)

		r.Get("/oncall", s.handleOnCall)

		r.Post("/incidents/{id}/ack", s.handleIncidentAck)
		r.Post("/incidents/{id}/resolve", s.handleIncidentResolve)
	})

	// The ack link is deliberately outside the authenticated tree: the signed
	// token is the authentication, and requiring a login here is exactly how
	// incidents go unacknowledged at 3am. Both methods are accepted because
	// some mail clients prefetch with GET.
	r.Get("/ack/{incident}/{user}/{token}", s.handleAckLink)
	r.Post("/ack/{incident}/{user}/{token}", s.handleAckLink)

	return r
}

// ─── On call ──────────────────────────────────────────────────────────────

type onCallEntry struct {
	Schedule string `json:"schedule"`
	Team     string `json:"team,omitempty"`
	User     string `json:"user,omitempty"`
	// Covered is false when nobody is on call, which is a real answer rather
	// than an error: the caller needs to be able to tell "nobody" apart from
	// "the schedule does not exist".
	Covered bool `json:"covered"`
}

type onCallInterval struct {
	Start string `json:"start"`
	End   string `json:"end"`
	User  string `json:"user,omitempty"`
	// Gap marks a span nobody covers, so a calendar renders holes rather than
	// silently omitting them.
	Gap bool `json:"gap,omitempty"`
}

type onCallResponse struct {
	At        string           `json:"at"`
	OnCall    []onCallEntry    `json:"oncall,omitempty"`
	Schedule  string           `json:"schedule,omitempty"`
	Intervals []onCallInterval `json:"intervals,omitempty"`
}

// handleOnCall answers GET /api/v1/oncall.
//
//	?team=platform          restrict to schedules owned by a team
//	?schedule=name          a single schedule
//	?at=<RFC3339>           an instant other than now
//	?days=N                 return the rota over N days instead of one answer
func (s *Server) handleOnCall(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	at := s.clk.Now()
	if raw := q.Get("at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("invalid at=%q: want RFC3339, e.g. 2026-08-17T09:00:00Z", raw))
			return
		}
		at = parsed
	}

	days := 0
	if raw := q.Get("days"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &days); err != nil || days < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid days=%q", raw))
			return
		}
		if days > maxOnCallDays {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("days=%d exceeds the %d day limit", days, maxOnCallDays))
			return
		}
	}

	names := s.selectSchedules(q.Get("schedule"), q.Get("team"))
	if len(names) == 0 {
		writeError(w, http.StatusNotFound, "no schedule matches that filter")
		return
	}

	if days > 0 {
		if len(names) != 1 {
			writeError(w, http.StatusBadRequest,
				"days= requires a single schedule; pass schedule=<name>")
			return
		}
		s.writeRota(w, r, names[0], at, at.AddDate(0, 0, days))
		return
	}

	resp := onCallResponse{At: at.UTC().Format(time.RFC3339)}
	for _, name := range names {
		res, err := s.resolver(r, name, at, at.Add(time.Nanosecond))
		if err != nil {
			s.log.Error("could not resolve on call", "schedule", name, "error", err)
			writeError(w, http.StatusInternalServerError, "could not resolve on call")
			return
		}
		user, ok := res.At(at)
		resp.OnCall = append(resp.OnCall, onCallEntry{
			Schedule: name,
			Team:     s.schedules[name].Team,
			User:     user,
			Covered:  ok,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) writeRota(w http.ResponseWriter, r *http.Request, name string, from, to time.Time) {
	res, err := s.resolver(r, name, from, to)
	if err != nil {
		s.log.Error("could not resolve rota", "schedule", name, "error", err)
		writeError(w, http.StatusInternalServerError, "could not resolve rota")
		return
	}

	out := onCallResponse{
		At:       from.UTC().Format(time.RFC3339),
		Schedule: name,
	}
	for _, iv := range res.Intervals(from, to) {
		out.Intervals = append(out.Intervals, onCallInterval{
			Start: iv.Start.UTC().Format(time.RFC3339),
			End:   iv.End.UTC().Format(time.RFC3339),
			User:  iv.UserID,
		})
	}
	for _, g := range res.Gaps(from, to) {
		out.Intervals = append(out.Intervals, onCallInterval{
			Start: g.Start.UTC().Format(time.RFC3339),
			End:   g.End.UTC().Format(time.RFC3339),
			Gap:   true,
		})
	}
	sort.Slice(out.Intervals, func(i, j int) bool {
		return out.Intervals[i].Start < out.Intervals[j].Start
	})

	writeJSON(w, http.StatusOK, out)
}

// resolver builds a resolver for one schedule, loading any overrides in force.
func (s *Server) resolver(r *http.Request, name string, from, to time.Time) (*schedule.Resolver, error) {
	sched := s.schedules[name]
	if s.overrides == nil {
		return schedule.NewResolver(sched, nil), nil
	}
	overrides, err := s.overrides.OverridesInWindow(r.Context(), name, from, to)
	if err != nil {
		return nil, err
	}
	return schedule.NewResolver(sched, overrides), nil
}

// selectSchedules applies the schedule and team filters, in that order.
func (s *Server) selectSchedules(scheduleName, team string) []string {
	var names []string
	for name, sched := range s.schedules {
		switch {
		case scheduleName != "" && name != scheduleName:
			continue
		case team != "" && sched.Team != team:
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
