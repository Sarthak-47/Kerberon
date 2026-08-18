package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/ack"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// Acknowledger is the escalation engine, narrowed to what the API needs.
type Acknowledger interface {
	Acknowledge(ctx context.Context, incidentID int64, userID string, via core.AckVia) error
}

// IncidentStore is the slice of the store the incident endpoints use.
type IncidentStore interface {
	Incident(ctx context.Context, id int64) (core.Incident, error)
	Resolve(ctx context.Context, id int64, by string, now time.Time) (bool, error)
}

// handleAckLink answers the one-tap link from a notification.
//
// It is deliberately unauthenticated: the signed token is the authentication,
// and requiring a login here is exactly how incidents go unacknowledged at 3am
// (spec section 8.4). The response is HTML because it is opened in a phone
// browser, not by a program.
func (s *Server) handleAckLink(w http.ResponseWriter, r *http.Request) {
	parsed, err := ack.ParseLinkPath(r.URL.Path)
	if err != nil {
		// A mangled link is usually a truncated notification rather than an
		// attack, so say what is wrong.
		s.ackPage(w, http.StatusBadRequest, "That link is not complete",
			"It may have been cut short by the app that displayed it. Open the incident in Kerberon instead.")
		return
	}

	if s.signer == nil || s.acker == nil {
		s.ackPage(w, http.StatusServiceUnavailable, "Acknowledgement is unavailable",
			"This server was started without acknowledgement configured.")
		return
	}

	inc, err := s.incidents.Incident(r.Context(), parsed.IncidentID)
	if err != nil {
		// Do not distinguish "no such incident" from "bad signature": that
		// would let someone probe which incident ids exist.
		s.ackPage(w, http.StatusForbidden, "That link is not valid",
			"It may have expired, or belong to a different incident.")
		return
	}

	step, ok := s.signer.VerifyAny(parsed.IncidentID, parsed.UserID, inc.CurrentStep, parsed.Token)
	if !ok {
		s.log.Warn("rejected an acknowledgement with an invalid token",
			"incident_id", parsed.IncidentID, "user", parsed.UserID, "remote", r.RemoteAddr)
		s.ackPage(w, http.StatusForbidden, "That link is not valid",
			"It may have expired, or belong to a different incident.")
		return
	}

	err = s.acker.Acknowledge(r.Context(), parsed.IncidentID, parsed.UserID, core.AckViaLink)
	switch {
	case err == nil:
		s.log.Info("incident acknowledged via link",
			"incident_id", parsed.IncidentID, "user", parsed.UserID, "step", step)
		s.ackPage(w, http.StatusOK, "Acknowledged",
			fmt.Sprintf("Incident %d is yours. Escalation has stopped. It is not resolved yet.",
				parsed.IncidentID))

	case isNotAcknowledgeable(err):
		// Tapping twice, or after someone else got there first, is ordinary.
		// Showing the current state is more useful than an error.
		current, _ := s.incidents.Incident(r.Context(), parsed.IncidentID)
		s.ackPage(w, http.StatusOK, "Already handled",
			describeIncidentState(current))

	default:
		s.log.Error("could not acknowledge incident",
			"incident_id", parsed.IncidentID, "error", err)
		s.ackPage(w, http.StatusInternalServerError, "Something went wrong",
			"The acknowledgement could not be recorded. Try the Kerberon UI.")
	}
}

// isNotAcknowledgeable matches the engine's sentinel without importing it,
// keeping the API free of a dependency on the escalation package.
func isNotAcknowledgeable(err error) bool {
	var target interface{ Error() string }
	if errors.As(err, &target) {
		return target.Error() == "incident is not awaiting acknowledgement"
	}
	return false
}

func describeIncidentState(inc core.Incident) string {
	switch inc.Status {
	case core.IncidentAcknowledged:
		if inc.AcknowledgedBy != "" {
			return fmt.Sprintf("Incident %d was already acknowledged by %s.", inc.ID, inc.AcknowledgedBy)
		}
		return fmt.Sprintf("Incident %d is already acknowledged.", inc.ID)
	case core.IncidentResolved:
		return fmt.Sprintf("Incident %d is already resolved.", inc.ID)
	case core.IncidentExpired:
		return fmt.Sprintf("Incident %d expired without an acknowledgement.", inc.ID)
	default:
		return fmt.Sprintf("Incident %d is no longer waiting to be acknowledged.", inc.ID)
	}
}

// ackPage renders the small page someone sees after tapping.
//
// It is inline rather than templated because it must render on a phone with no
// network beyond this one request: no stylesheet, no font, no script.
func (s *Server) ackPage(w http.ResponseWriter, status int, heading, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	accent := "#1a7f37"
	if status >= 400 {
		accent = "#b42318"
	}

	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — Kerberon</title>
</head>
<body style="margin:0;font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:#f6f7f9;color:#1f2328">
<main style="max-width:32rem;margin:12vh auto;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 3px rgba(0,0,0,.12)">
<h1 style="margin:0 0 .5rem;font-size:1.5rem;color:%s">%s</h1>
<p style="margin:0;color:#4a5056">%s</p>
</main>
</body></html>
`, html.EscapeString(heading), accent, html.EscapeString(heading), html.EscapeString(detail))
}

// ─── Incident API ─────────────────────────────────────────────────────────

// handleIncidentAck acknowledges through the authenticated API, for tooling
// and for the UI.
func (s *Server) handleIncidentAck(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentIDParam(w, r)
	if !ok {
		return
	}
	if s.acker == nil {
		writeError(w, http.StatusServiceUnavailable, "acknowledgement is not configured")
		return
	}

	user := r.URL.Query().Get("user")
	if user == "" {
		user = "api"
	}

	err := s.acker.Acknowledge(r.Context(), id, user, core.AckViaAPI)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{
			"incident_id": id, "status": "acknowledged", "by": user,
		})
	case isNotAcknowledgeable(err):
		// A no-op, not a failure: an ack arriving after a resolution is
		// ordinary. 409 says "the state moved on" without implying a bug.
		writeError(w, http.StatusConflict, "incident is not awaiting acknowledgement")
	default:
		s.log.Error("acknowledge failed", "incident_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not acknowledge incident")
	}
}

// handleIncidentResolve closes an incident by hand.
func (s *Server) handleIncidentResolve(w http.ResponseWriter, r *http.Request) {
	id, ok := incidentIDParam(w, r)
	if !ok {
		return
	}
	if s.incidents == nil {
		writeError(w, http.StatusServiceUnavailable, "incident storage is not configured")
		return
	}

	user := r.URL.Query().Get("user")
	if user == "" {
		user = "api"
	}

	closed, err := s.incidents.Resolve(r.Context(), id, user, s.clk.Now())
	if err != nil {
		s.log.Error("resolve failed", "incident_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "could not resolve incident")
		return
	}
	if !closed {
		writeError(w, http.StatusConflict, "incident is already closed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"incident_id": id, "status": "resolved", "by": user,
	})
}

func incidentIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid incident id %q", raw))
		return 0, false
	}
	return id, true
}
