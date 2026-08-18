// Package config parses and validates kerberon.yaml.
//
// Configuration is declarative and lives in the user's git repository: teams,
// users, schedules, escalation policies, routing rules and channel credentials.
// Mutable state — incidents, timers, notifications, overrides — lives in SQLite
// and never here. A user can delete kerberon.db and lose history but not their
// setup (spec section 4.2).
//
// Validation is deliberately strict and reports line numbers, because the
// alternative to failing a config check in CI is failing at 3am.
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// Config is a parsed kerberon.yaml.
type Config struct {
	Server             Server             `yaml:"server"`
	Database           Database           `yaml:"database"`
	Users              []User             `yaml:"users"`
	Teams              []Team             `yaml:"teams"`
	Schedules          []Schedule         `yaml:"schedules"`
	EscalationPolicies []EscalationPolicy `yaml:"escalation_policies"`
	Routes             []Route            `yaml:"routes"`
	Channels           Channels           `yaml:"channels"`
	Notifications      Notifications      `yaml:"notifications"`
}

type Server struct {
	Listen string `yaml:"listen"`
	// ExternalURL is required: ack links are built from it, and a link that
	// points at localhost is useless on the phone of the person being paged.
	ExternalURL string `yaml:"external_url"`
	// SecretKey signs ack tokens (HMAC-SHA256).
	SecretKey string `yaml:"secret_key"`
	// IngestToken authenticates the alert-receiving endpoints.
	IngestToken string `yaml:"ingest_token"`
}

type Database struct {
	Path string `yaml:"path"`
}

type User struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
	// Timezone is an IANA name. A fixed offset such as +05:30 is not a
	// timezone and is rejected (spec section 7.2).
	Timezone string   `yaml:"timezone"`
	Contacts Contacts `yaml:"contacts"`
}

// Contacts maps a channel name to that user's address on it.
type Contacts map[string]string

type Team struct {
	Name    string   `yaml:"name"`
	Members []string `yaml:"members"`
}

type Schedule struct {
	Name     string  `yaml:"name"`
	Team     string  `yaml:"team"`
	Timezone string  `yaml:"timezone"`
	Layers   []Layer `yaml:"layers"`
}

// LayerType distinguishes a rotating layer from a fixed restriction window.
type LayerType string

const (
	LayerRotation LayerType = "rotation"
	// LayerRestriction confines a layer to certain hours, e.g. business hours.
	LayerRestriction LayerType = "restriction"
)

type Layer struct {
	Name         string    `yaml:"name"`
	Type         LayerType `yaml:"type"`
	Participants []string  `yaml:"participants"`
	// Rotation is daily or weekly.
	Rotation string  `yaml:"rotation"`
	Handoff  Handoff `yaml:"handoff"`
	// Restriction confines this layer to certain hours, e.g. business hours.
	// Required for a layer of type restriction, optional on a rotation.
	// Outside the window the layer puts nobody on call, so a schedule built
	// only from restricted layers has coverage gaps by construction — which is
	// exactly what kerberon validate is meant to catch.
	Restriction *Restriction `yaml:"restriction"`
}

// Restriction is a recurring window during which a layer applies.
type Restriction struct {
	// Days are weekday names. Empty means every day.
	Days []string `yaml:"days"`
	// Start and End are 24-hour HH:MM in the layer's timezone. An End at or
	// before Start describes a window crossing midnight, e.g. 18:00 to 09:00
	// for an after-hours layer.
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

// Handoff is when one participant takes over from the next, expressed as
// wall-clock time in the schedule's timezone.
type Handoff struct {
	Day string `yaml:"day"`
	// Time is "HH:MM" in 24-hour form.
	Time string `yaml:"time"`
	// Timezone overrides the schedule's timezone for this handoff, if set.
	Timezone string `yaml:"timezone"`
}

type EscalationPolicy struct {
	Name string `yaml:"name"`
	// Repeat is how many times the whole policy loops before the incident
	// expires.
	Repeat int `yaml:"repeat"`
	// AckTimeout resumes escalation if an acknowledged incident is not
	// resolved within this window — the "acknowledged and fell back asleep"
	// case. Zero disables it.
	AckTimeout Duration `yaml:"ack_timeout"`
	Steps      []Step   `yaml:"steps"`
}

type Step struct {
	// Delay is measured from the previous step, not from incident creation.
	Delay Duration `yaml:"delay"`
	// Targets are resolved at the moment the step fires, not when the incident
	// is created, so an incident spanning a handoff pages whoever is on call
	// then (spec section 8.1).
	Targets  []Target `yaml:"targets"`
	Channels []string `yaml:"channels"`
}

type Route struct {
	// Name is optional but recommended. It is part of the group key, so a
	// route with an explicit name keeps its open incidents across edits to
	// its match criteria. Left blank, a stable name is derived from the match
	// criteria, team and policy — which means editing any of those starts new
	// incidents rather than reusing the old group.
	Name   string            `yaml:"name"`
	Match  map[string]string `yaml:"match"`
	Team   string            `yaml:"team"`
	Policy string            `yaml:"policy"`
	// GroupBy lists the labels whose values define an incident's group key.
	GroupBy []string `yaml:"group_by"`
	// GroupWait lets a cascade arrive before the first page, so the
	// notification says "12 services down" rather than paging twelve times.
	GroupWait Duration `yaml:"group_wait"`
	// GroupInterval is the minimum gap before a new notification for an
	// already-open incident that gained alerts.
	GroupInterval Duration `yaml:"group_interval"`
	// ResolveGrace is how long all-resolved must hold before the incident
	// closes. This is what stops a flapping alert producing a
	// page-resolve-page storm.
	ResolveGrace Duration `yaml:"resolve_grace"`
	// VolatileLabels are excluded from the fingerprint. Including a pod name
	// means a rescheduled pod looks like a new alert, which is the most common
	// cause of duplicate paging in Kubernetes.
	VolatileLabels []string `yaml:"volatile_labels"`
}

type Channels struct {
	Ntfy     *NtfyChannel     `yaml:"ntfy"`
	Telegram *TelegramChannel `yaml:"telegram"`
	Email    *EmailChannel    `yaml:"email"`
	Webhook  *WebhookChannel  `yaml:"webhook"`
}

type NtfyChannel struct {
	DefaultServer string `yaml:"default_server"`
}

type TelegramChannel struct {
	BotToken string `yaml:"bot_token"`
}

type EmailChannel struct {
	SMTPHost string `yaml:"smtp_host"`
	SMTPPort int    `yaml:"smtp_port"`
	From     string `yaml:"from"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type WebhookChannel struct {
	URL string `yaml:"url"`
}

type Notifications struct {
	// FallbackChannel is used when every channel configured for a user fails.
	FallbackChannel string `yaml:"fallback_channel"`
	MaxAttempts     int    `yaml:"max_attempts"`
}

// ─── Target ───────────────────────────────────────────────────────────────

// TargetKind is what a policy step points at. It is an alias for the core type
// because an incident's policy snapshot carries these values, and the
// escalation engine reads them back without config in the picture.
type TargetKind = core.TargetKind

const (
	TargetSchedule = core.TargetSchedule
	TargetUser     = core.TargetUser
	TargetTeam     = core.TargetTeam
)

// Target is a step's recipient, written as "schedule:platform-primary",
// "user:sarthak" or "team:platform".
type Target struct {
	Kind TargetKind
	Name string
	// line records where this appeared, for error messages.
	line int
}

func (t Target) String() string { return string(t.Kind) + ":" + t.Name }

func (t *Target) UnmarshalYAML(node *yaml.Node) error {
	t.line = node.Line

	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("target must be a string like schedule:platform-primary")
	}

	kind, name, found := cut(raw, ":")
	if !found || name == "" {
		return fmt.Errorf("target %q must be schedule:<name>, user:<id> or team:<name>", raw)
	}
	switch TargetKind(kind) {
	case TargetSchedule, TargetUser, TargetTeam:
		t.Kind = TargetKind(kind)
		t.Name = name
		return nil
	default:
		return fmt.Errorf("target %q has unknown kind %q; want schedule, user or team", raw, kind)
	}
}

// cut is strings.Cut, kept local to avoid an import for one call.
func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// ─── Duration ─────────────────────────────────────────────────────────────

// Duration accepts Go duration strings ("30s", "5m", "2h") and a bare 0.
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string like 30s, 5m or 2h")
	}
	// `delay: 0` is idiomatic in the spec's examples and parses as the string
	// "0", which time.ParseDuration rejects for lacking a unit.
	if raw == "0" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: want a value like 30s, 5m or 2h", raw)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", raw)
	}
	*d = Duration(parsed)
	return nil
}

// ─── Loading ──────────────────────────────────────────────────────────────

// Load reads, expands and validates the config at path.
//
// The returned error is a *Errors when validation fails, which carries every
// problem found rather than only the first — fixing a config one error per run
// is miserable.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, path)
}

// Parse is Load for an in-memory document. path is used only in messages.
func Parse(data []byte, path string) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, &Errors{Path: path, Items: []Error{{
			Message: "config is empty",
		}}}
	}

	// Expand ${VAR} on the node tree rather than the raw bytes. Expanding text
	// before parsing would let a value containing ':' or a newline change the
	// document's structure.
	errs := &Errors{Path: path}
	expandNode(root.Content[0], errs)
	if len(errs.Items) > 0 {
		return nil, errs
	}

	var cfg Config
	if err := decodeStrict(root.Content[0], &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(root.Content[0], path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// decodeStrict rejects unknown fields, so a typo like "timezome" fails loudly
// instead of silently leaving the real field at its zero value.
//
// yaml.Node.Decode has no strict mode, so the expanded tree is re-encoded and
// read back through a Decoder that does. Line numbers are taken from the
// original tree, which is kept separately, so nothing is lost by round-tripping.
func decodeStrict(node *yaml.Node, out any) error {
	buf, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("re-encode config for strict decoding: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// applyDefaults fills the values the spec documents as defaults.
func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:8080"
	}
	if c.Database.Path == "" {
		c.Database.Path = "./kerberon.db"
	}
	if c.Notifications.MaxAttempts == 0 {
		c.Notifications.MaxAttempts = 5
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.GroupWait == 0 {
			r.GroupWait = Duration(30 * time.Second)
		}
		if r.GroupInterval == 0 {
			r.GroupInterval = Duration(5 * time.Minute)
		}
		if r.ResolveGrace == 0 {
			r.ResolveGrace = Duration(2 * time.Minute)
		}
		if len(r.VolatileLabels) == 0 {
			// Spec section 6.2. A rescheduled pod must not look like a new alert.
			r.VolatileLabels = []string{
				"timestamp", "value", "instance_id", "pod", "container_id", "trace_id",
			}
		}
	}
}

// ─── Lookups ──────────────────────────────────────────────────────────────

// UserByID returns the user with the given id.
func (c *Config) UserByID(id string) (*User, bool) {
	for i := range c.Users {
		if c.Users[i].ID == id {
			return &c.Users[i], true
		}
	}
	return nil, false
}

// TeamByName returns the team with the given name.
func (c *Config) TeamByName(name string) (*Team, bool) {
	for i := range c.Teams {
		if c.Teams[i].Name == name {
			return &c.Teams[i], true
		}
	}
	return nil, false
}

// ScheduleByName returns the schedule with the given name.
func (c *Config) ScheduleByName(name string) (*Schedule, bool) {
	for i := range c.Schedules {
		if c.Schedules[i].Name == name {
			return &c.Schedules[i], true
		}
	}
	return nil, false
}

// PolicyByName returns the escalation policy with the given name.
func (c *Config) PolicyByName(name string) (*EscalationPolicy, bool) {
	for i := range c.EscalationPolicies {
		if c.EscalationPolicies[i].Name == name {
			return &c.EscalationPolicies[i], true
		}
	}
	return nil, false
}
