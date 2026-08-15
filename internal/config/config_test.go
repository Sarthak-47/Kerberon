package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
)

// setEnv sets the variables the example config references.
func setEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"KERBERON_SECRET":       "0123456789abcdef0123456789abcdef",
		"KERBERON_INGEST_TOKEN": "ingest-token-value",
		"TELEGRAM_BOT_TOKEN":    "12345:bot-token",
		"SMTP_USER":             "smtp-user",
		"SMTP_PASS":             "smtp-pass",
	} {
		t.Setenv(k, v)
	}
}

// The shipped example must always validate. If this fails, the quickstart in
// the README is broken for every new user.
func TestExampleConfigIsValid(t *testing.T) {
	setEnv(t)
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "kerberon.yaml"))
	if err != nil {
		t.Fatalf("examples/kerberon.yaml should validate:\n%v", err)
	}

	if got := len(cfg.Users); got != 2 {
		t.Errorf("users = %d, want 2", got)
	}
	if cfg.Server.SecretKey != "0123456789abcdef0123456789abcdef" {
		t.Errorf("secret_key was not expanded: %q", cfg.Server.SecretKey)
	}
	if got := cfg.Routes[0].GroupWait.Std(); got != 30*time.Second {
		t.Errorf("group_wait = %v, want 30s", got)
	}
	if got := cfg.EscalationPolicies[0].AckTimeout.Std(); got != 30*time.Minute {
		t.Errorf("ack_timeout = %v, want 30m", got)
	}
	if got := cfg.EscalationPolicies[0].Steps[0].Targets[0].String(); got != "schedule:platform-primary" {
		t.Errorf("first target = %q", got)
	}
}

// base is a minimal valid config that individual tests mutate to isolate one
// failure each.
const base = `
server:
  external_url: "https://kerberon.example.com"
  secret_key: "s"
  ingest_token: "t"
users:
  - id: sarthak
    name: Sarthak
    timezone: "Asia/Kolkata"
    contacts:
      ntfy: "https://ntfy.sh/x"
teams:
  - name: platform
    members: [sarthak]
schedules:
  - name: platform-primary
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: base
        type: rotation
        participants: [sarthak]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
escalation_policies:
  - name: critical-24x7
    steps:
      - delay: 0
        targets: [schedule:platform-primary]
        channels: [ntfy]
routes:
  - match:
      severity: critical
    team: platform
    policy: critical-24x7
    group_by: [alertname]
channels:
  ntfy:
    default_server: "https://ntfy.sh"
`

func TestBaseConfigIsValid(t *testing.T) {
	if _, err := config.Parse([]byte(base), "test.yaml"); err != nil {
		t.Fatalf("base config should validate:\n%v", err)
	}
}

// mustFail parses and asserts the error mentions want.
func mustFail(t *testing.T, yaml, want string) *config.Errors {
	t.Helper()
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatalf("expected a validation error mentioning %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error did not mention %q:\n%v", want, err)
	}
	errs, ok := err.(*config.Errors)
	if !ok {
		return nil
	}
	return errs
}

func TestUnknownReferencesAreRejected(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			"team member is not a user",
			strings.Replace(base, "members: [sarthak]", "members: [nobody]", 1),
			`unknown user "nobody"`,
		},
		{
			"schedule participant is not a user",
			strings.Replace(base, "participants: [sarthak]", "participants: [ghost]", 1),
			`unknown user "ghost"`,
		},
		{
			"schedule references an unknown team",
			strings.Replace(base, "team: platform\n    timezone", "team: nosuch\n    timezone", 1),
			`unknown team "nosuch"`,
		},
		{
			"policy targets an unknown schedule",
			strings.Replace(base, "targets: [schedule:platform-primary]", "targets: [schedule:missing]", 1),
			`unknown schedule "missing"`,
		},
		{
			"route references an unknown policy",
			strings.Replace(base, "policy: critical-24x7", "policy: nonexistent", 1),
			`unknown escalation policy "nonexistent"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { mustFail(t, c.yaml, c.want) })
	}
}

// A fixed offset cannot express DST, which is exactly the bug the spec calls
// out: a rotation built on it silently drifts twice a year.
func TestFixedOffsetTimezoneIsRejectedWithAnExplanation(t *testing.T) {
	bad := strings.Replace(base, `timezone: "Asia/Kolkata"`, `timezone: "+05:30"`, 1)
	errs := mustFail(t, bad, "+05:30")
	if errs != nil && !strings.Contains(errs.Error(), "daylight saving") {
		t.Errorf("error should explain why an offset is not a timezone:\n%v", errs)
	}
}

func TestTimezoneValidation(t *testing.T) {
	cases := []struct {
		name string
		tz   string
		ok   bool
	}{
		{"IANA name", "Asia/Kolkata", true},
		{"UTC", "UTC", true},
		{"southern hemisphere", "Australia/Sydney", true},
		{"misspelled", "Asia/Calcuta", false},
		{"abbreviation", "IST", false},
		{"Local is not portable", "Local", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := strings.Replace(base, `timezone: "Asia/Kolkata"`, `timezone: "`+c.tz+`"`, 1)
			_, err := config.Parse([]byte(y), "test.yaml")
			if c.ok && err != nil {
				t.Fatalf("timezone %q should be accepted:\n%v", c.tz, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("timezone %q should be rejected", c.tz)
			}
		})
	}
}

// Discovering a missing contact at 3am is the most common on-call setup
// failure, so validate catches it in CI instead.
func TestUserPagedOnAChannelTheyLackIsRejected(t *testing.T) {
	y := strings.Replace(base, "channels: [ntfy]", "channels: [ntfy, telegram]", 1)
	y = strings.Replace(y, "channels:\n  ntfy:", "channels:\n  telegram:\n    bot_token: x\n  ntfy:", 1)
	mustFail(t, y, `user "sarthak" has no telegram contact`)
}

func TestChannelUsedButNotConfiguredIsRejected(t *testing.T) {
	y := strings.Replace(base, "channels: [ntfy]", "channels: [email]", 1)
	y = strings.Replace(y, `      ntfy: "https://ntfy.sh/x"`, `      email: "a@b.com"`, 1)
	mustFail(t, y, "not configured under channels:")
}

// sms and voice parse but are not deliverable until v1.1. Accepting them
// silently would mean a policy that looks louder than it is.
func TestDeferredChannelsAreRejected(t *testing.T) {
	y := strings.Replace(base, "channels: [ntfy]", "channels: [ntfy, sms]", 1)
	mustFail(t, y, "not implemented until v1.1")
}

func TestMissingRequiredServerFields(t *testing.T) {
	cases := []struct{ name, from, want string }{
		{"external_url", `external_url: "https://kerberon.example.com"`, "server.external_url"},
		{"secret_key", `secret_key: "s"`, "server.secret_key"},
		{"ingest_token", `ingest_token: "t"`, "server.ingest_token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mustFail(t, strings.Replace(base, c.from, "", 1), c.want)
		})
	}
}

func TestExternalURLMustBeAbsolute(t *testing.T) {
	y := strings.Replace(base, `external_url: "https://kerberon.example.com"`,
		`external_url: "kerberon.example.com"`, 1)
	mustFail(t, y, "external_url")
}

func TestDuplicateIdentifiersAreRejected(t *testing.T) {
	dup := strings.Replace(base, `teams:
  - name: platform
    members: [sarthak]`, `teams:
  - name: platform
    members: [sarthak]
  - name: platform
    members: [sarthak]`, 1)
	mustFail(t, dup, "duplicate team")
}

func TestUserWithNoContactsIsRejected(t *testing.T) {
	y := strings.Replace(base, `    contacts:
      ntfy: "https://ntfy.sh/x"`, "    contacts: {}", 1)
	mustFail(t, y, "can never be paged")
}

func TestUnknownFieldIsRejected(t *testing.T) {
	// A typo in a key would otherwise leave the real field at its zero value.
	y := strings.Replace(base, `    timezone: "Asia/Kolkata"
    contacts:`, `    timezome: "Asia/Kolkata"
    contacts:`, 1)
	_, err := config.Parse([]byte(y), "test.yaml")
	if err == nil {
		t.Fatal("a misspelled field should be rejected")
	}
	if !strings.Contains(err.Error(), "timezome") {
		t.Errorf("error should name the offending field:\n%v", err)
	}
}

func TestRouteWithoutGroupByIsRejected(t *testing.T) {
	mustFail(t, strings.Replace(base, "    group_by: [alertname]", "", 1), "group_by")
}

// ─── Environment expansion ────────────────────────────────────────────────

func TestUnsetEnvironmentVariableIsAnError(t *testing.T) {
	y := strings.Replace(base, `secret_key: "s"`, `secret_key: "${DEFINITELY_NOT_SET_XYZ}"`, 1)
	mustFail(t, y, "DEFINITELY_NOT_SET_XYZ")
}

func TestEnvironmentExpansionForms(t *testing.T) {
	t.Setenv("KERB_TEST_VALUE", "expanded")

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"braced", "${KERB_TEST_VALUE}", "expanded"},
		{"bare", "$KERB_TEST_VALUE", "expanded"},
		{"embedded", "prefix-${KERB_TEST_VALUE}-suffix", "prefix-expanded-suffix"},
		{"escaped dollar", "$$KERB_TEST_VALUE", "$KERB_TEST_VALUE"},
		{"no reference", "plain", "plain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := strings.Replace(base, `ingest_token: "t"`, `ingest_token: "`+c.in+`"`, 1)
			cfg, err := config.Parse([]byte(y), "test.yaml")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if cfg.Server.IngestToken != c.want {
				t.Errorf("ingest_token = %q, want %q", cfg.Server.IngestToken, c.want)
			}
		})
	}
}

// Expansion happens on the parsed tree, so a secret containing a colon or a
// newline cannot alter the document's structure.
func TestExpandedValueCannotCorruptDocumentStructure(t *testing.T) {
	t.Setenv("KERB_NASTY", "value: injected\nusers: []")

	y := strings.Replace(base, `ingest_token: "t"`, `ingest_token: "${KERB_NASTY}"`, 1)
	cfg, err := config.Parse([]byte(y), "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Server.IngestToken != "value: injected\nusers: []" {
		t.Errorf("token = %q; the value should be preserved verbatim", cfg.Server.IngestToken)
	}
	if len(cfg.Users) != 1 {
		t.Errorf("users = %d, want 1; the injected value altered the document", len(cfg.Users))
	}
}

// ─── Durations, targets, defaults ─────────────────────────────────────────

func TestDurationParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"30s", 30 * time.Second, true},
		{"5m", 5 * time.Minute, true},
		{"2h", 2 * time.Hour, true},
		{"1h30m", 90 * time.Minute, true},
		{"5", 0, false},      // no unit
		{"-5m", 0, false},    // negative
		{"banana", 0, false}, // nonsense
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			y := strings.Replace(base, "    group_by: [alertname]",
				"    group_by: [alertname]\n    group_wait: \""+c.in+"\"", 1)
			cfg, err := config.Parse([]byte(y), "test.yaml")
			if !c.ok {
				if err == nil {
					t.Fatalf("duration %q should be rejected", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("duration %q should parse: %v", c.in, err)
			}
			// 0 falls back to the documented 30s default.
			want := c.want
			if want == 0 {
				want = 30 * time.Second
			}
			if got := cfg.Routes[0].GroupWait.Std(); got != want {
				t.Errorf("group_wait = %v, want %v", got, want)
			}
		})
	}
}

func TestMalformedTargetIsRejected(t *testing.T) {
	for _, bad := range []string{"platform-primary", "group:platform", "schedule:"} {
		t.Run(bad, func(t *testing.T) {
			y := strings.Replace(base, "targets: [schedule:platform-primary]",
				"targets: ["+bad+"]", 1)
			if _, err := config.Parse([]byte(y), "test.yaml"); err == nil {
				t.Fatalf("target %q should be rejected", bad)
			}
		})
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	cfg, err := config.Parse([]byte(base), "test.yaml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Server.Listen; got != "0.0.0.0:8080" {
		t.Errorf("listen = %q, want the default", got)
	}
	if got := cfg.Database.Path; got != "./kerberon.db" {
		t.Errorf("database.path = %q, want the default", got)
	}
	if got := cfg.Notifications.MaxAttempts; got != 5 {
		t.Errorf("max_attempts = %d, want 5", got)
	}
	r := cfg.Routes[0]
	if r.GroupWait.Std() != 30*time.Second {
		t.Errorf("group_wait = %v, want 30s", r.GroupWait)
	}
	if r.GroupInterval.Std() != 5*time.Minute {
		t.Errorf("group_interval = %v, want 5m", r.GroupInterval)
	}
	if r.ResolveGrace.Std() != 2*time.Minute {
		t.Errorf("resolve_grace = %v, want 2m", r.ResolveGrace)
	}
	// The default volatile set exists because a rescheduled pod must not look
	// like a new alert.
	if !contains(r.VolatileLabels, "pod") {
		t.Errorf("volatile_labels = %v, want the default set including pod", r.VolatileLabels)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// ─── Error reporting ──────────────────────────────────────────────────────

// Fixing a config one error per run is miserable, and this command is meant to
// run in CI.
func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	y := strings.Replace(base, "members: [sarthak]", "members: [ghost]", 1)
	y = strings.Replace(y, "policy: critical-24x7", "policy: missing-policy", 1)
	y = strings.Replace(y, `timezone: "Asia/Kolkata"`, `timezone: "Mars/Olympus"`, 1)

	errs := mustFail(t, y, "unknown user")
	if errs == nil {
		t.Fatal("expected a *config.Errors")
	}
	if errs.Len() < 3 {
		t.Errorf("reported %d problems, want at least 3:\n%v", errs.Len(), errs)
	}
	for _, want := range []string{"ghost", "missing-policy", "Mars/Olympus"} {
		if !strings.Contains(errs.Error(), want) {
			t.Errorf("report omitted %q:\n%v", want, errs)
		}
	}
}

func TestErrorsCarryLineNumbers(t *testing.T) {
	y := strings.Replace(base, "members: [sarthak]", "members: [ghost]", 1)
	errs := mustFail(t, y, "ghost")
	if errs == nil {
		t.Fatal("expected a *config.Errors")
	}
	var withLine int
	for _, it := range errs.Items {
		if it.Line > 0 {
			withLine++
		}
	}
	if withLine == 0 {
		t.Errorf("no error carried a line number:\n%v", errs)
	}
	if !strings.Contains(errs.Error(), "line ") {
		t.Errorf("rendered report should show line numbers:\n%v", errs)
	}
}
