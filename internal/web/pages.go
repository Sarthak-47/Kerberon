package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

type base struct {
	Title string
	Nav   string
}

// ─── Now ──────────────────────────────────────────────────────────────────

type onCallRow struct {
	Schedule string
	Team     string
	User     string
	Covered  bool
}

type nowPage struct {
	base
	OnCall     []onCallRow
	Incidents  []core.Incident
	Heartbeats []core.Heartbeat
}

// handleNow renders the page people leave open on a wall display: who is on
// call, what is burning, and whether the dead-man's switches are alive.
func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.clk.Now()

	page := nowPage{base: base{Title: "Now", Nav: "now"}}

	names := make([]string, 0, len(s.schedules))
	for name := range s.schedules {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		sched := s.schedules[name]
		overrides, err := s.db.OverridesInWindow(ctx, name, now, now.Add(time.Nanosecond))
		if err != nil {
			s.fail(w, "overrides", err)
			return
		}
		user, ok := schedule.NewResolver(sched, overrides).At(now)
		page.OnCall = append(page.OnCall, onCallRow{
			Schedule: name, Team: sched.Team, User: user, Covered: ok,
		})
	}

	// Expired incidents belong here too: nobody answered, and that is the
	// state most in need of a human noticing.
	incidents, err := s.db.Incidents(ctx, []core.IncidentStatus{
		core.IncidentTriggered, core.IncidentAcknowledged, core.IncidentExpired,
	}, 100)
	if err != nil {
		s.fail(w, "incidents", err)
		return
	}
	page.Incidents = incidents

	hbs, err := s.db.Heartbeats(ctx)
	if err != nil {
		s.fail(w, "heartbeats", err)
		return
	}
	page.Heartbeats = hbs

	s.render(w, "now", page)
}

// ─── Incidents ────────────────────────────────────────────────────────────

type incidentsPage struct {
	base
	Incidents []core.Incident
}

func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	var statuses []core.IncidentStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		st := core.IncidentStatus(raw)
		if !st.Valid() {
			http.Error(w, "unknown status", http.StatusBadRequest)
			return
		}
		statuses = []core.IncidentStatus{st}
	}

	incidents, err := s.db.Incidents(r.Context(), statuses, 200)
	if err != nil {
		s.fail(w, "incidents", err)
		return
	}
	s.render(w, "incidents", incidentsPage{
		base:      base{Title: "Incidents", Nav: "incidents"},
		Incidents: incidents,
	})
}

type incidentPage struct {
	base
	Incident      core.Incident
	Events        []core.Event
	Notifications []core.Notification
	Alerts        []core.Alert
}

// handleIncident renders the full timeline: created, who was notified on which
// channel, escalated, acknowledged by whom, resolved.
func (s *Server) handleIncident(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid incident id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	inc, err := s.db.Incident(ctx, id)
	if err != nil {
		http.Error(w, "no such incident", http.StatusNotFound)
		return
	}
	events, err := s.db.Events(ctx, id)
	if err != nil {
		s.fail(w, "events", err)
		return
	}
	notifs, err := s.db.NotificationsForIncident(ctx, id)
	if err != nil {
		s.fail(w, "notifications", err)
		return
	}
	alerts, err := s.db.AlertsForIncident(ctx, id)
	if err != nil {
		s.fail(w, "alerts", err)
		return
	}

	s.render(w, "incident", incidentPage{
		base:          base{Title: fmt.Sprintf("Incident %d", id), Nav: "incidents"},
		Incident:      inc,
		Events:        events,
		Notifications: notifs,
		Alerts:        alerts,
	})
}

// ─── Schedules ────────────────────────────────────────────────────────────

type span struct {
	Start string
	End   string
	User  string
	Gap   bool
}

type schedView struct {
	Name     string
	Team     string
	Timezone string
	Spans    []span
}

type schedulesPage struct {
	base
	Schedules []schedView
}

// handleSchedules renders the calendar, gaps included.
//
// Gaps are shown inline rather than in a separate list so a hole is impossible
// to miss while reading the rota, which is the moment someone would notice it.
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	from := s.clk.Now()
	to := from.AddDate(0, 0, calendarDays)

	names := make([]string, 0, len(s.schedules))
	for name := range s.schedules {
		names = append(names, name)
	}
	sort.Strings(names)

	page := schedulesPage{base: base{Title: "Schedules", Nav: "schedules"}}

	for _, name := range names {
		sched := s.schedules[name]
		overrides, err := s.db.OverridesInWindow(ctx, name, from, to)
		if err != nil {
			s.fail(w, "overrides", err)
			return
		}
		res := schedule.NewResolver(sched, overrides)

		view := schedView{Name: name, Team: sched.Team, Timezone: sched.Location.String()}
		type raw struct {
			start, end time.Time
			user       string
			gap        bool
		}
		var rows []raw
		for _, iv := range res.Intervals(from, to) {
			rows = append(rows, raw{iv.Start, iv.End, iv.UserID, false})
		}
		for _, g := range res.Gaps(from, to) {
			rows = append(rows, raw{g.Start, g.End, "", true})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].start.Before(rows[j].start) })

		for _, x := range rows {
			view.Spans = append(view.Spans, span{
				Start: x.start.In(sched.Location).Format("Mon 02 Jan 15:04"),
				End:   x.end.In(sched.Location).Format("Mon 02 Jan 15:04"),
				User:  x.user,
				Gap:   x.gap,
			})
		}
		page.Schedules = append(page.Schedules, view)
	}
	s.render(w, "schedules", page)
}

// ─── Config ───────────────────────────────────────────────────────────────

type gapRow struct {
	Schedule string
	Start    string
	End      string
	Duration string
}

type userRow struct {
	ID       string
	Name     string
	Timezone string
	Contacts string
}

type teamRow struct {
	Name    string
	Members string
}

type policyRow struct {
	Name       string
	Repeat     int
	AckTimeout string
	Steps      string
}

type routeRow struct {
	Name      string
	Match     string
	Team      string
	Policy    string
	GroupBy   string
	GroupWait string
}

type configPage struct {
	base
	Gaps     []gapRow
	Users    []userRow
	Teams    []teamRow
	Policies []policyRow
	Routes   []routeRow
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	page := configPage{base: base{Title: "Config", Nav: "config"}}

	// Coverage is checked without overrides, matching kerberon validate: a
	// rotation that only has cover because someone happens to be filling in
	// next Thursday is still a broken rotation.
	gaps, err := schedule.CheckCoverage(s.cfg, s.clk.Now(), schedule.CoverageWindow)
	if err != nil {
		s.fail(w, "coverage", err)
		return
	}
	for i, g := range gaps {
		if i == 50 {
			break // the point is made; the list is not the product
		}
		page.Gaps = append(page.Gaps, gapRow{
			Schedule: g.Schedule,
			Start:    g.Interval.Start.Format("2006-01-02 15:04 MST"),
			End:      g.Interval.End.Format("2006-01-02 15:04 MST"),
			Duration: g.Interval.Duration().String(),
		})
	}

	for _, u := range s.cfg.Users {
		chans := make([]string, 0, len(u.Contacts))
		for ch := range u.Contacts {
			chans = append(chans, ch)
		}
		sort.Strings(chans)
		// Addresses are deliberately not shown: this page is readable by
		// anyone who can reach the UI, and a topic URL is a capability.
		page.Users = append(page.Users, userRow{
			ID: u.ID, Name: u.Name, Timezone: u.Timezone,
			Contacts: strings.Join(chans, ", "),
		})
	}
	for _, t := range s.cfg.Teams {
		page.Teams = append(page.Teams, teamRow{
			Name: t.Name, Members: strings.Join(t.Members, ", "),
		})
	}
	for _, p := range s.cfg.EscalationPolicies {
		steps := make([]string, 0, len(p.Steps))
		for i, st := range p.Steps {
			targets := make([]string, 0, len(st.Targets))
			for _, tg := range st.Targets {
				targets = append(targets, tg.String())
			}
			steps = append(steps, fmt.Sprintf("%d: +%s %s via %s",
				i, st.Delay, strings.Join(targets, ","), strings.Join(st.Channels, ",")))
		}
		page.Policies = append(page.Policies, policyRow{
			Name: p.Name, Repeat: p.Repeat, AckTimeout: p.AckTimeout.String(),
			Steps: strings.Join(steps, " | "),
		})
	}
	for _, rt := range s.cfg.Routes {
		keys := make([]string, 0, len(rt.Match))
		for k := range rt.Match {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+rt.Match[k])
		}
		page.Routes = append(page.Routes, routeRow{
			Name: rt.Name, Match: strings.Join(parts, " "),
			Team: rt.Team, Policy: rt.Policy,
			GroupBy: strings.Join(rt.GroupBy, ","), GroupWait: rt.GroupWait.String(),
		})
	}

	s.render(w, "config", page)
}

// ─── Settings ─────────────────────────────────────────────────────────────

type contactRow struct {
	User        string
	Channel     string
	Destination string
	Tested      bool
	TestError   string
}

type settingsPage struct {
	base
	Contacts     []contactRow
	ExternalURL  string
	DatabasePath string
	Version      string
	Channels     string
}

func (s *Server) settingsData(testedUser, testedChannel, testErr string) settingsPage {
	page := settingsPage{
		base:         base{Title: "Settings", Nav: "settings"},
		ExternalURL:  s.cfg.Server.ExternalURL,
		DatabasePath: s.cfg.Database.Path,
		Version:      s.version,
	}

	names := make([]string, 0, len(s.channels))
	for name := range s.channels {
		names = append(names, string(name))
	}
	sort.Strings(names)
	page.Channels = strings.Join(names, ", ")
	if page.Channels == "" {
		page.Channels = "none configured"
	}

	for _, u := range s.cfg.Users {
		chans := make([]string, 0, len(u.Contacts))
		for ch := range u.Contacts {
			chans = append(chans, ch)
		}
		sort.Strings(chans)
		for _, ch := range chans {
			row := contactRow{User: u.ID, Channel: ch, Destination: u.Contacts[ch]}
			if u.ID == testedUser && ch == testedChannel {
				row.Tested = true
				row.TestError = testErr
			}
			page.Contacts = append(page.Contacts, row)
		}
	}
	return page
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings", s.settingsData("", "", ""))
}

// handleTestNotification sends a real notification through a real channel.
//
// It deliberately uses the same delivery path as a page rather than a
// simulation: the entire value of the button is proving that this contact, on
// this channel, actually reaches this person's device.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user := r.FormValue("user")
	channel := r.FormValue("channel")

	testErr := s.sendTest(r, user, channel)
	if testErr != "" {
		s.log.Warn("test notification failed", "user", user, "channel", channel, "error", testErr)
	} else {
		s.log.Info("test notification sent", "user", user, "channel", channel)
	}
	s.render(w, "settings", s.settingsData(user, channel, testErr))
}

func (s *Server) sendTest(r *http.Request, user, channel string) string {
	ch, ok := s.channels[core.Channel(channel)]
	if !ok {
		return fmt.Sprintf("channel %q is not configured on this server", channel)
	}

	var dest string
	for _, u := range s.cfg.Users {
		if u.ID == user {
			dest = u.Contacts[channel]
		}
	}
	if dest == "" {
		return fmt.Sprintf("%s has no %s address", user, channel)
	}

	err := ch.Send(r.Context(), notify.Message{
		Destination: dest,
		Title:       "Kerberon test",
		Body: "This is a test from Kerberon. If you can read this, " +
			"this contact works and a real page will reach you.",
		Severity: core.SeverityInfo,
	})
	if err != nil {
		return err.Error()
	}
	return ""
}
