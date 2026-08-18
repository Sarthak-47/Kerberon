package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/store"
)

// HeartbeatRecorder is the slice of the store the ping endpoint needs.
type HeartbeatRecorder interface {
	RecordPing(ctx context.Context, token string, at time.Time) (core.Heartbeat, error)
}

// handleHeartbeatPing records that a job is still alive.
//
// It is authenticated by the token in the path rather than the ingest token,
// because the thing pinging is usually a one-line curl in a crontab that
// cannot carry a header. The token is unguessable for the same reason it
// matters: anyone who could guess it could keep a dead job looking alive.
//
// Both GET and POST are accepted, since the shortest thing to put in a crontab
// is a bare curl.
func (s *Server) handleHeartbeatPing(w http.ResponseWriter, r *http.Request) {
	if s.heartbeats == nil {
		writeError(w, http.StatusServiceUnavailable, "heartbeats are not configured")
		return
	}

	token := chi.URLParam(r, "token")
	if token == "" {
		writeError(w, http.StatusBadRequest, "missing heartbeat token")
		return
	}

	before, err := s.heartbeats.RecordPing(r.Context(), token, s.clk.Now())
	if errors.Is(err, store.ErrUnknownHeartbeat) {
		// Deliberately vague: a precise answer would let someone enumerate
		// which tokens exist.
		writeError(w, http.StatusNotFound, "unknown heartbeat")
		return
	}
	if err != nil {
		s.log.Error("could not record heartbeat ping", "error", err)
		writeError(w, http.StatusInternalServerError, "could not record ping")
		return
	}

	if before.State == core.HeartbeatMissing {
		// The job came back. Worth saying plainly, because the incident it
		// raised is still open and somebody is probably looking at it.
		s.log.Info("heartbeat recovered", "heartbeat", before.Name, "team", before.Team)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"heartbeat": before.Name,
		"status":    "ok",
		"recovered": before.State == core.HeartbeatMissing,
	})
}
