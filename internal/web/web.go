// Package web serves the small read-mostly UI.
//
// Server-rendered with html/template, embedded in the binary. There is no
// build step, no node_modules and no static assets to deploy: the UI is mostly
// tables and forms, and a JavaScript toolchain for that would cost more than
// it returns (spec section 4.3).
//
// The one page that does more than display is Settings, which sends a test
// notification. Most on-call setup failures are a wrong contact discovered at
// the worst possible moment, so being able to check before an incident depends
// on it is not a nicety (spec section 10).
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

//go:embed templates/*.html
var templateFS embed.FS

// calendarDays is how far the schedules page looks ahead.
const calendarDays = 30

// Store is the slice of the database the UI reads.
type Store interface {
	Incidents(ctx context.Context, statuses []core.IncidentStatus, limit int) ([]core.Incident, error)
	Incident(ctx context.Context, id int64) (core.Incident, error)
	Events(ctx context.Context, incidentID int64) ([]core.Event, error)
	NotificationsForIncident(ctx context.Context, incidentID int64) ([]core.Notification, error)
	AlertsForIncident(ctx context.Context, incidentID int64) ([]core.Alert, error)
	Heartbeats(ctx context.Context) ([]core.Heartbeat, error)
	OverridesInWindow(ctx context.Context, scheduleName string, from, to time.Time) ([]core.Override, error)
}

// Server renders the UI.
type Server struct {
	db        Store
	cfg       *config.Config
	schedules map[string]*schedule.Schedule
	channels  map[core.Channel]notify.Channel
	clk       clock.Clock
	version   string
	log       *slog.Logger

	tpl map[string]*template.Template
}

// Options configures a Server.
type Options struct {
	// Channels enables the Settings test button. Without them the page still
	// renders and says which channels are unavailable.
	Channels []notify.Channel
	Version  string
	Logger   *slog.Logger
}

// New parses the templates and returns a Server.
//
// Parsing at construction rather than per request means a broken template is a
// startup failure, not a 500 discovered by whoever opens the page during an
// incident.
func New(db Store, cfg *config.Config, schedules map[string]*schedule.Schedule, clk clock.Clock, opts Options) (*Server, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	byName := make(map[core.Channel]notify.Channel, len(opts.Channels))
	for _, c := range opts.Channels {
		byName[c.Name()] = c
	}

	s := &Server{
		db: db, cfg: cfg, schedules: schedules, channels: byName,
		clk: clk, version: opts.Version, log: log,
		tpl: map[string]*template.Template{},
	}

	for _, page := range []string{"now", "incidents", "incident", "schedules", "config", "settings"} {
		t, err := template.New("layout.html").Funcs(s.funcs()).ParseFS(templateFS,
			"templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", page, err)
		}
		s.tpl[page] = t
	}
	return s, nil
}

// Routes mounts the UI under /ui.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.handleNow)
	r.Get("/incidents", s.handleIncidents)
	r.Get("/incidents/{id}", s.handleIncident)
	r.Get("/schedules", s.handleSchedules)
	r.Get("/config", s.handleConfig)
	r.Get("/settings", s.handleSettings)
	r.Post("/settings/test", s.handleTestNotification)
	return r
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"fmtTime": func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
		"fmtTimePtr": func(t *time.Time) string {
			if t == nil {
				return ""
			}
			return t.Format("2006-01-02 15:04:05 MST")
		},
		"since": func(t any) string {
			var at time.Time
			switch v := t.(type) {
			case time.Time:
				at = v
			case *time.Time:
				if v == nil {
					return "never"
				}
				at = *v
			}
			return humanDuration(s.clk.Now().Sub(at))
		},
		"labels": func(l core.Labels) string {
			keys := make([]string, 0, len(l))
			for k := range l {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+"="+l[k])
			}
			return strings.Join(parts, " ")
		},
		"severityClass": func(sev core.Severity) string {
			switch sev {
			case core.SeverityCritical:
				return "crit"
			case core.SeverityWarning:
				return "warn"
			default:
				return "info"
			}
		},
		"statusClass": func(st core.IncidentStatus) string {
			switch st {
			case core.IncidentTriggered:
				return "crit"
			case core.IncidentAcknowledged:
				return "warn"
			case core.IncidentResolved:
				return "ok"
			case core.IncidentExpired:
				// Nobody answered. That is worse than triggered, and the UI
				// should not make it look calm.
				return "crit"
			default:
				return "info"
			}
		},
		"heartbeatClass": func(st core.HeartbeatState) string {
			switch st {
			case core.HeartbeatHealthy:
				return "ok"
			case core.HeartbeatMissing:
				return "crit"
			default:
				return "info"
			}
		},
		"notifClass": func(st core.NotificationState) string {
			switch st {
			case core.NotifSent:
				return "ok"
			case core.NotifDead:
				return "crit"
			case core.NotifFailed:
				return "warn"
			default:
				return "info"
			}
		},
	}
}

// humanDuration renders an age the way someone glancing at a wall display
// reads it.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.tpl[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		// The status is already written by now, so this can only be logged.
		s.log.Error("could not render page", "page", page, "error", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log.Error("ui query failed", "what", what, "error", err)
	http.Error(w, "something went wrong; check the server log", http.StatusInternalServerError)
}
