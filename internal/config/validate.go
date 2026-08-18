package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	// The timezone database is compiled in. Without it, rotations break on
	// minimal container images and on Windows (spec section 7.2).
	_ "time/tzdata"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// Error is a single validation problem.
type Error struct {
	// Line is the 1-indexed line in the config, or 0 if not attributable.
	Line int
	// Field is a dotted path such as "schedules[0].layers[1].participants".
	Field   string
	Message string
}

// Errors is every problem found in one config. Validation collects all of them
// rather than stopping at the first: fixing a config one error per run is
// miserable, and this command is meant to run in CI.
type Errors struct {
	Path  string
	Items []Error
}

func (e *Errors) Add(line int, field, msg string) {
	e.Items = append(e.Items, Error{Line: line, Field: field, Message: msg})
}

func (e *Errors) Len() int { return len(e.Items) }

func (e *Errors) Error() string {
	if len(e.Items) == 0 {
		return "config is valid"
	}
	// Report in file order so the reader can work top to bottom.
	items := make([]Error, len(e.Items))
	copy(items, e.Items)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Line < items[j].Line })

	var b strings.Builder
	noun := "problems"
	if len(items) == 1 {
		noun = "problem"
	}
	fmt.Fprintf(&b, "%s: %d %s\n", e.Path, len(items), noun)
	for _, it := range items {
		switch {
		case it.Line > 0 && it.Field != "":
			fmt.Fprintf(&b, "  line %d (%s): %s\n", it.Line, it.Field, it.Message)
		case it.Line > 0:
			fmt.Fprintf(&b, "  line %d: %s\n", it.Line, it.Message)
		case it.Field != "":
			fmt.Fprintf(&b, "  %s: %s\n", it.Field, it.Message)
		default:
			fmt.Fprintf(&b, "  %s\n", it.Message)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ─── Node navigation, for line numbers ────────────────────────────────────

// nodeAt walks a document by path, where each element is a string (mapping key)
// or an int (sequence index). It returns nil if the path does not exist.
func nodeAt(root *yaml.Node, path ...any) *yaml.Node {
	cur := root
	for _, seg := range path {
		if cur == nil {
			return nil
		}
		switch key := seg.(type) {
		case string:
			if cur.Kind != yaml.MappingNode {
				return nil
			}
			var found *yaml.Node
			// Mapping content alternates key, value.
			for i := 0; i+1 < len(cur.Content); i += 2 {
				if cur.Content[i].Value == key {
					found = cur.Content[i+1]
					break
				}
			}
			cur = found
		case int:
			if cur.Kind != yaml.SequenceNode || key < 0 || key >= len(cur.Content) {
				return nil
			}
			cur = cur.Content[key]
		default:
			return nil
		}
	}
	return cur
}

// lineAt is the line of the node at path, or 0.
func lineAt(root *yaml.Node, path ...any) int {
	if n := nodeAt(root, path...); n != nil {
		return n.Line
	}
	return 0
}

// ─── Validation ───────────────────────────────────────────────────────────

// knownChannels are the channels a policy step may name. sms and voice are
// accepted in config but not deliverable until v1.1 (spec section 8.5).
var knownChannels = map[string]bool{
	"ntfy": true, "telegram": true, "email": true, "webhook": true,
	"sms": true, "voice": true,
}

var deferredChannels = map[string]bool{"sms": true, "voice": true}

var validDays = map[string]bool{
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true,
	"friday": true, "saturday": true, "sunday": true,
}

// Validate checks the config and returns a *Errors if anything is wrong.
// root may be nil, in which case errors carry no line numbers.
func (c *Config) Validate(root *yaml.Node, path string) error {
	errs := &Errors{Path: path}

	c.validateServer(root, errs)
	c.validateUsers(root, errs)
	c.validateTeams(root, errs)
	c.validateSchedules(root, errs)
	c.validatePolicies(root, errs)
	c.validateRoutes(root, errs)
	c.validateHeartbeats(root, errs)
	c.validateNotifications(root, errs)

	if len(errs.Items) > 0 {
		return errs
	}
	return nil
}

func (c *Config) validateServer(root *yaml.Node, errs *Errors) {
	if c.Server.ExternalURL == "" {
		errs.Add(lineAt(root, "server"), "server.external_url",
			"required: ack links are built from it, and a link the paged person cannot open is useless")
	} else {
		u, err := url.Parse(c.Server.ExternalURL)
		switch {
		case err != nil:
			errs.Add(lineAt(root, "server", "external_url"), "server.external_url",
				fmt.Sprintf("not a valid URL: %v", err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs.Add(lineAt(root, "server", "external_url"), "server.external_url",
				fmt.Sprintf("scheme is %q, want http or https", u.Scheme))
		case u.Host == "":
			errs.Add(lineAt(root, "server", "external_url"), "server.external_url",
				"missing host")
		}
	}

	if c.Server.SecretKey == "" {
		errs.Add(lineAt(root, "server"), "server.secret_key",
			"required: it signs ack tokens, and an empty key makes them forgeable")
	}
	if c.Server.IngestToken == "" {
		errs.Add(lineAt(root, "server"), "server.ingest_token",
			"required: it authenticates the alert endpoints, which are otherwise open to anyone who can reach them")
	}
}

func (c *Config) validateUsers(root *yaml.Node, errs *Errors) {
	if len(c.Users) == 0 {
		errs.Add(0, "users", "at least one user is required; there is nobody to page")
		return
	}
	seen := map[string]int{}
	for i, u := range c.Users {
		field := fmt.Sprintf("users[%d]", i)
		line := lineAt(root, "users", i)

		if u.ID == "" {
			errs.Add(line, field+".id", "required")
		} else if prev, dup := seen[u.ID]; dup {
			errs.Add(line, field+".id",
				fmt.Sprintf("duplicate user id %q, first defined at users[%d]", u.ID, prev))
		} else {
			seen[u.ID] = i
		}

		validateTimezone(root, errs, u.Timezone, field+".timezone", []any{"users", i, "timezone"}, line, false)

		if len(u.Contacts) == 0 {
			errs.Add(line, field+".contacts",
				fmt.Sprintf("user %q has no contact methods and can never be paged", u.ID))
			continue
		}
		for ch := range u.Contacts {
			if !knownChannels[ch] {
				errs.Add(lineAt(root, "users", i, "contacts"), field+".contacts",
					fmt.Sprintf("unknown channel %q; want one of %s", ch, channelList()))
			}
		}
	}
}

func (c *Config) validateTeams(root *yaml.Node, errs *Errors) {
	seen := map[string]int{}
	for i, t := range c.Teams {
		field := fmt.Sprintf("teams[%d]", i)
		line := lineAt(root, "teams", i)

		if t.Name == "" {
			errs.Add(line, field+".name", "required")
		} else if prev, dup := seen[t.Name]; dup {
			errs.Add(line, field+".name",
				fmt.Sprintf("duplicate team %q, first defined at teams[%d]", t.Name, prev))
		} else {
			seen[t.Name] = i
		}

		if len(t.Members) == 0 {
			errs.Add(line, field+".members",
				fmt.Sprintf("team %q has no members; a policy targeting it would page nobody", t.Name))
		}
		for j, m := range t.Members {
			if _, ok := c.UserByID(m); !ok {
				errs.Add(lineAt(root, "teams", i, "members", j), fmt.Sprintf("%s.members[%d]", field, j),
					fmt.Sprintf("unknown user %q", m))
			}
		}
	}
}

func (c *Config) validateSchedules(root *yaml.Node, errs *Errors) {
	seen := map[string]int{}
	for i, s := range c.Schedules {
		field := fmt.Sprintf("schedules[%d]", i)
		line := lineAt(root, "schedules", i)

		if s.Name == "" {
			errs.Add(line, field+".name", "required")
		} else if prev, dup := seen[s.Name]; dup {
			errs.Add(line, field+".name",
				fmt.Sprintf("duplicate schedule %q, first defined at schedules[%d]", s.Name, prev))
		} else {
			seen[s.Name] = i
		}

		if s.Team != "" {
			if _, ok := c.TeamByName(s.Team); !ok {
				errs.Add(lineAt(root, "schedules", i, "team"), field+".team",
					fmt.Sprintf("unknown team %q", s.Team))
			}
		}

		validateTimezone(root, errs, s.Timezone, field+".timezone", []any{"schedules", i, "timezone"}, line, true)

		if len(s.Layers) == 0 {
			errs.Add(line, field+".layers",
				fmt.Sprintf("schedule %q has no layers and can never put anyone on call", s.Name))
			continue
		}
		for j, l := range s.Layers {
			c.validateLayer(root, errs, i, j, l)
		}
	}
}

func (c *Config) validateLayer(root *yaml.Node, errs *Errors, si, li int, l Layer) {
	field := fmt.Sprintf("schedules[%d].layers[%d]", si, li)
	line := lineAt(root, "schedules", si, "layers", li)

	switch l.Type {
	case LayerRotation, LayerRestriction:
	case "":
		errs.Add(line, field+".type", "required: want rotation or restriction")
	default:
		errs.Add(line, field+".type",
			fmt.Sprintf("unknown layer type %q; want rotation or restriction", l.Type))
	}

	if len(l.Participants) == 0 {
		errs.Add(line, field+".participants", "at least one participant is required")
	}
	for k, p := range l.Participants {
		if _, ok := c.UserByID(p); !ok {
			errs.Add(lineAt(root, "schedules", si, "layers", li, "participants", k),
				fmt.Sprintf("%s.participants[%d]", field, k),
				fmt.Sprintf("unknown user %q", p))
		}
	}

	if l.Type == LayerRotation {
		switch l.Rotation {
		case "daily", "weekly":
		case "":
			errs.Add(line, field+".rotation", "required for a rotation layer: want daily or weekly")
		default:
			errs.Add(line, field+".rotation",
				fmt.Sprintf("unknown rotation %q; want daily or weekly", l.Rotation))
		}
	}

	hline := lineAt(root, "schedules", si, "layers", li, "handoff")
	if hline == 0 {
		hline = line
	}
	if l.Handoff.Day != "" && !validDays[strings.ToLower(l.Handoff.Day)] {
		errs.Add(hline, field+".handoff.day",
			fmt.Sprintf("unknown day %q; want monday through sunday", l.Handoff.Day))
	}
	if l.Rotation == "weekly" && l.Handoff.Day == "" {
		errs.Add(hline, field+".handoff.day", "required for a weekly rotation")
	}
	if l.Handoff.Time != "" {
		if _, err := time.Parse("15:04", l.Handoff.Time); err != nil {
			errs.Add(hline, field+".handoff.time",
				fmt.Sprintf("invalid time %q; want 24-hour HH:MM such as 09:00", l.Handoff.Time))
		}
	} else if l.Type == LayerRotation {
		errs.Add(hline, field+".handoff.time", "required: want 24-hour HH:MM such as 09:00")
	}
	if l.Handoff.Timezone != "" {
		validateTimezone(root, errs, l.Handoff.Timezone, field+".handoff.timezone",
			[]any{"schedules", si, "layers", li, "handoff", "timezone"}, hline, true)
	}

	validateRestriction(root, errs, si, li, l, field, line)
}

func validateRestriction(root *yaml.Node, errs *Errors, si, li int, l Layer, field string, fallbackLine int) {
	rline := lineAt(root, "schedules", si, "layers", li, "restriction")
	if rline == 0 {
		rline = fallbackLine
	}

	if l.Restriction == nil {
		if l.Type == LayerRestriction {
			errs.Add(fallbackLine, field+".restriction",
				"required for a layer of type restriction: give days, start and end")
		}
		return
	}

	r := l.Restriction
	if r.Start == "" || r.End == "" {
		errs.Add(rline, field+".restriction",
			"both start and end are required, as 24-hour HH:MM")
	}
	for _, name := range []struct{ label, value string }{{"start", r.Start}, {"end", r.End}} {
		if name.value == "" {
			continue
		}
		if _, err := time.Parse("15:04", name.value); err != nil {
			errs.Add(rline, field+".restriction."+name.label,
				fmt.Sprintf("invalid time %q; want 24-hour HH:MM such as 09:00", name.value))
		}
	}
	if r.Start != "" && r.Start == r.End {
		errs.Add(rline, field+".restriction",
			fmt.Sprintf("start and end are both %q, which covers no time at all", r.Start))
	}
	for _, d := range r.Days {
		if !validDays[strings.ToLower(d)] {
			errs.Add(rline, field+".restriction.days",
				fmt.Sprintf("unknown day %q; want monday through sunday", d))
		}
	}
}

func (c *Config) validatePolicies(root *yaml.Node, errs *Errors) {
	seen := map[string]int{}
	for i, p := range c.EscalationPolicies {
		field := fmt.Sprintf("escalation_policies[%d]", i)
		line := lineAt(root, "escalation_policies", i)

		if p.Name == "" {
			errs.Add(line, field+".name", "required")
		} else if prev, dup := seen[p.Name]; dup {
			errs.Add(line, field+".name",
				fmt.Sprintf("duplicate policy %q, first defined at escalation_policies[%d]", p.Name, prev))
		} else {
			seen[p.Name] = i
		}

		if p.Repeat < 0 {
			errs.Add(line, field+".repeat", "must not be negative")
		}
		if len(p.Steps) == 0 {
			errs.Add(line, field+".steps",
				fmt.Sprintf("policy %q has no steps and would page nobody", p.Name))
			continue
		}
		for j, s := range p.Steps {
			c.validateStep(root, errs, i, j, s)
		}
	}
}

func (c *Config) validateStep(root *yaml.Node, errs *Errors, pi, si int, s Step) {
	field := fmt.Sprintf("escalation_policies[%d].steps[%d]", pi, si)
	line := lineAt(root, "escalation_policies", pi, "steps", si)

	if len(s.Targets) == 0 {
		errs.Add(line, field+".targets", "at least one target is required")
	}
	for _, t := range s.Targets {
		tline := t.line
		if tline == 0 {
			tline = line
		}
		var known bool
		switch t.Kind {
		case TargetSchedule:
			_, known = c.ScheduleByName(t.Name)
		case TargetUser:
			_, known = c.UserByID(t.Name)
		case TargetTeam:
			_, known = c.TeamByName(t.Name)
		}
		if !known {
			errs.Add(tline, field+".targets",
				fmt.Sprintf("unknown %s %q", t.Kind, t.Name))
		}
	}

	if len(s.Channels) == 0 {
		errs.Add(line, field+".channels", "at least one channel is required")
	}
	for _, ch := range s.Channels {
		if !knownChannels[ch] {
			errs.Add(line, field+".channels",
				fmt.Sprintf("unknown channel %q; want one of %s", ch, channelList()))
			continue
		}
		if deferredChannels[ch] {
			errs.Add(line, field+".channels",
				fmt.Sprintf("channel %q is not implemented until v1.1 and would never deliver", ch))
			continue
		}
		if !c.channelConfigured(ch) {
			errs.Add(line, field+".channels",
				fmt.Sprintf("channel %q is used here but not configured under channels:", ch))
		}
		// Every user this step can reach needs an address on this channel.
		// Discovering a missing contact at 3am is the most common on-call
		// setup failure (spec section 10).
		for _, u := range c.reachableUsers(s.Targets) {
			if _, ok := u.Contacts[ch]; !ok {
				errs.Add(line, field+".channels",
					fmt.Sprintf("user %q has no %s contact but this step pages them on it", u.ID, ch))
			}
		}
	}
}

// reachableUsers is every user a set of targets could page. Schedule targets
// contribute their layer participants, since resolution happens at fire time
// and any participant may be the one on call.
func (c *Config) reachableUsers(targets []Target) []*User {
	seen := map[string]bool{}
	var out []*User

	add := func(id string) {
		if seen[id] {
			return
		}
		if u, ok := c.UserByID(id); ok {
			seen[id] = true
			out = append(out, u)
		}
	}

	for _, t := range targets {
		switch t.Kind {
		case TargetUser:
			add(t.Name)
		case TargetTeam:
			if team, ok := c.TeamByName(t.Name); ok {
				for _, m := range team.Members {
					add(m)
				}
			}
		case TargetSchedule:
			if s, ok := c.ScheduleByName(t.Name); ok {
				for _, l := range s.Layers {
					for _, p := range l.Participants {
						add(p)
					}
				}
			}
		}
	}
	return out
}

func (c *Config) channelConfigured(name string) bool {
	switch name {
	case "ntfy":
		return c.Channels.Ntfy != nil
	case "telegram":
		return c.Channels.Telegram != nil
	case "email":
		return c.Channels.Email != nil
	case "webhook":
		return c.Channels.Webhook != nil
	}
	return false
}

func (c *Config) validateRoutes(root *yaml.Node, errs *Errors) {
	if len(c.Routes) == 0 {
		errs.Add(0, "routes",
			"at least one route is required; incoming alerts would match nothing and never page")
		return
	}
	for i, r := range c.Routes {
		field := fmt.Sprintf("routes[%d]", i)
		line := lineAt(root, "routes", i)

		if len(r.Match) == 0 {
			errs.Add(line, field+".match",
				"required: a route with no match criteria would capture every alert")
		}
		if r.Team == "" {
			errs.Add(line, field+".team", "required")
		} else if _, ok := c.TeamByName(r.Team); !ok {
			errs.Add(lineAt(root, "routes", i, "team"), field+".team",
				fmt.Sprintf("unknown team %q", r.Team))
		}
		if r.Policy == "" {
			errs.Add(line, field+".policy", "required")
		} else if _, ok := c.PolicyByName(r.Policy); !ok {
			errs.Add(lineAt(root, "routes", i, "policy"), field+".policy",
				fmt.Sprintf("unknown escalation policy %q", r.Policy))
		}
		if len(r.GroupBy) == 0 {
			errs.Add(line, field+".group_by",
				"required: without it every alert forms its own incident and grouping does nothing")
		}
	}
}

func (c *Config) validateHeartbeats(root *yaml.Node, errs *Errors) {
	seen := map[string]int{}
	for i, h := range c.Heartbeats {
		field := fmt.Sprintf("heartbeats[%d]", i)
		line := lineAt(root, "heartbeats", i)

		if h.Name == "" {
			errs.Add(line, field+".name", "required")
		} else if prev, dup := seen[h.Name]; dup {
			errs.Add(line, field+".name",
				fmt.Sprintf("duplicate heartbeat %q, first defined at heartbeats[%d]", h.Name, prev))
		} else {
			seen[h.Name] = i
		}

		if h.ExpectedInterval.Std() <= 0 {
			errs.Add(line, field+".expected_interval",
				"required and must be positive, e.g. 5m")
		}
		if h.Team == "" {
			errs.Add(line, field+".team", "required")
		} else if _, ok := c.TeamByName(h.Team); !ok {
			errs.Add(line, field+".team", fmt.Sprintf("unknown team %q", h.Team))
		}
		if h.Severity != "" {
			if sev := core.Severity(strings.ToLower(h.Severity)); !sev.Valid() {
				errs.Add(line, field+".severity",
					fmt.Sprintf("unknown severity %q; want critical, warning or info", h.Severity))
			}
		}
	}
}

func (c *Config) validateNotifications(root *yaml.Node, errs *Errors) {
	if c.Notifications.MaxAttempts < 1 {
		errs.Add(lineAt(root, "notifications", "max_attempts"), "notifications.max_attempts",
			"must be at least 1")
	}
	fb := c.Notifications.FallbackChannel
	if fb == "" {
		return
	}
	line := lineAt(root, "notifications", "fallback_channel")
	if !knownChannels[fb] {
		errs.Add(line, "notifications.fallback_channel",
			fmt.Sprintf("unknown channel %q; want one of %s", fb, channelList()))
		return
	}
	if !c.channelConfigured(fb) {
		errs.Add(line, "notifications.fallback_channel",
			fmt.Sprintf("channel %q is the fallback but is not configured under channels:", fb))
	}
}

// validateTimezone rejects anything that is not a loadable IANA name. A fixed
// offset such as +05:30 is not a timezone: it cannot express DST, so a rotation
// defined against it silently drifts twice a year (spec section 7.2).
func validateTimezone(root *yaml.Node, errs *Errors, tz, field string, path []any, fallbackLine int, required bool) {
	line := lineAt(root, path...)
	if line == 0 {
		line = fallbackLine
	}
	if tz == "" {
		if required {
			errs.Add(line, field, "required: an IANA name such as Asia/Kolkata")
		}
		return
	}
	if tz == "Local" {
		errs.Add(line, field,
			`"Local" depends on the host and is not portable; name the zone explicitly, e.g. Asia/Kolkata`)
		return
	}
	if _, err := time.LoadLocation(tz); err != nil {
		msg := fmt.Sprintf("unknown IANA timezone %q", tz)
		if strings.HasPrefix(tz, "+") || strings.HasPrefix(tz, "-") {
			msg += ": a fixed UTC offset cannot express daylight saving, so a rotation using it drifts twice a year. Use a zone name such as Asia/Kolkata"
		}
		errs.Add(line, field, msg)
	}
}

func channelList() string {
	names := make([]string, 0, len(knownChannels))
	for n := range knownChannels {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
