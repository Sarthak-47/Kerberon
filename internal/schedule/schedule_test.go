package schedule_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/schedule"
)

// build parses a config and returns the named schedule's resolver.
func build(t *testing.T, scheduleYAML string, overrides ...core.Override) (*schedule.Resolver, *schedule.Schedule) {
	t.Helper()

	full := `
server:
  external_url: "https://k.example.com"
  secret_key: "s"
  ingest_token: "t"
users:
  - id: sarthak
    name: Sarthak
    timezone: "Asia/Kolkata"
    contacts: {ntfy: "https://ntfy.sh/a"}
  - id: priya
    name: Priya
    timezone: "Asia/Kolkata"
    contacts: {ntfy: "https://ntfy.sh/b"}
  - id: arun
    name: Arun
    timezone: "Asia/Kolkata"
    contacts: {ntfy: "https://ntfy.sh/c"}
teams:
  - name: platform
    members: [sarthak, priya, arun]
escalation_policies:
  - name: p
    steps:
      - delay: 0
        targets: [team:platform]
        channels: [ntfy]
routes:
  - match: {severity: critical}
    team: platform
    policy: p
    group_by: [alertname]
channels:
  ntfy:
    default_server: "https://ntfy.sh"
` + scheduleYAML

	cfg, err := config.Parse([]byte(full), "test.yaml")
	if err != nil {
		t.Fatalf("config:\n%v", err)
	}
	schedules, err := schedule.FromConfig(cfg)
	if err != nil {
		t.Fatalf("build schedules: %v", err)
	}
	s, ok := schedules["oncall"]
	if !ok {
		t.Fatalf("schedule %q not built", "oncall")
	}
	return schedule.NewResolver(s, overrides), s
}

const weeklyTwo = `
schedules:
  - name: oncall
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: base
        type: rotation
        participants: [sarthak, priya]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
`

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// ─── Basic rotation ───────────────────────────────────────────────────────

func TestRotationAlternatesEachWeek(t *testing.T) {
	r, _ := build(t, weeklyTwo)

	// 2026-08-17 is a Monday. Handoff is 09:00 Asia/Kolkata = 03:30 UTC.
	start := mustTime(t, "2026-08-17T04:00:00Z")
	first, ok := r.At(start)
	if !ok {
		t.Fatal("nobody on call")
	}

	second, ok := r.At(start.AddDate(0, 0, 7))
	if !ok {
		t.Fatal("nobody on call the following week")
	}
	if first == second {
		t.Errorf("the same person (%s) is on call two weeks running", first)
	}

	third, _ := r.At(start.AddDate(0, 0, 14))
	if third != first {
		t.Errorf("a two-person rotation should return to %s after two weeks, got %s", first, third)
	}
}

// Rotation position is anchored to a fixed reference, so who is on call must
// not depend on when the process started or which window is rendered.
func TestRotationPositionIsIndependentOfTheQueryWindow(t *testing.T) {
	r, _ := build(t, weeklyTwo)
	at := mustTime(t, "2026-08-19T12:00:00Z")

	want, _ := r.At(at)

	for _, window := range []struct{ from, to time.Time }{
		{at.AddDate(0, 0, -1), at.AddDate(0, 0, 1)},
		{at.AddDate(0, 0, -30), at.AddDate(0, 0, 30)},
		{at.AddDate(0, -6, 0), at.AddDate(0, 6, 0)},
	} {
		intervals := r.Intervals(window.from, window.to)
		var got string
		for _, iv := range intervals {
			if iv.Contains(at) {
				got = iv.UserID
			}
		}
		if got != want {
			t.Errorf("window %s..%s says %q, point lookup says %q",
				window.from.Format("2006-01-02"), window.to.Format("2006-01-02"), got, want)
		}
	}
}

func TestHandoffBoundaryIsHalfOpen(t *testing.T) {
	r, _ := build(t, weeklyTwo)

	// 09:00 Asia/Kolkata on Monday 2026-08-24 is 03:30 UTC.
	handoff := mustTime(t, "2026-08-24T03:30:00Z")

	before, _ := r.At(handoff.Add(-time.Second))
	at, _ := r.At(handoff)
	if before == at {
		t.Errorf("the handoff instant still reports %s; the incoming person should already hold it", before)
	}
}

// ─── Intervals ────────────────────────────────────────────────────────────

func TestIntervalsCoverTheWindowContiguously(t *testing.T) {
	r, _ := build(t, weeklyTwo)
	from := mustTime(t, "2026-08-01T00:00:00Z")
	to := mustTime(t, "2026-09-01T00:00:00Z")

	intervals := r.Intervals(from, to)
	if len(intervals) == 0 {
		t.Fatal("no intervals produced")
	}
	if !intervals[0].Start.Equal(from) {
		t.Errorf("first interval starts at %s, want %s", intervals[0].Start, from)
	}
	if last := intervals[len(intervals)-1]; !last.End.Equal(to) {
		t.Errorf("last interval ends at %s, want %s", last.End, to)
	}
	for i := 1; i < len(intervals); i++ {
		if !intervals[i].Start.Equal(intervals[i-1].End) {
			t.Errorf("gap or overlap between %s and %s", intervals[i-1], intervals[i])
		}
		if intervals[i].UserID == intervals[i-1].UserID {
			t.Errorf("adjacent intervals both assigned to %s; they should have merged",
				intervals[i].UserID)
		}
	}
}

func TestIntervalsAndAtAgree(t *testing.T) {
	r, _ := build(t, weeklyTwo)
	from := mustTime(t, "2026-01-01T00:00:00Z")
	to := from.AddDate(0, 3, 0)

	for _, iv := range r.Intervals(from, to) {
		for _, probe := range []time.Time{
			iv.Start,
			iv.Start.Add(iv.Duration() / 2),
			iv.End.Add(-time.Second),
		} {
			got, ok := r.At(probe)
			if !ok || got != iv.UserID {
				t.Fatalf("At(%s) = %q (%v), but the interval says %q",
					probe.Format(time.RFC3339), got, ok, iv.UserID)
			}
		}
	}
}

// The property from spec section 11: over any 400-day window the on-call
// intervals cover it exactly once — no gaps, no overlaps. Sampling could not
// establish this; a hole shorter than the sample step would pass unseen, which
// is precisely the failure mode at a DST boundary.
func TestNoGapsOrOverlapsOver400Days(t *testing.T) {
	zones := []string{"America/New_York", "Europe/London", "Australia/Sydney", "Asia/Kolkata"}
	rotations := []string{"weekly", "daily"}
	participants := []string{"[sarthak, priya]", "[sarthak, priya, arun]"}

	for _, zone := range zones {
		for _, rot := range rotations {
			for _, ps := range participants {
				name := fmt.Sprintf("%s/%s/%s", zone, rot, ps)
				t.Run(name, func(t *testing.T) {
					day := "monday"
					r, _ := build(t, fmt.Sprintf(`
schedules:
  - name: oncall
    team: platform
    timezone: %q
    layers:
      - name: base
        type: rotation
        participants: %s
        rotation: %s
        handoff:
          day: %s
          time: "09:00"
`, zone, ps, rot, day))

					// Start deliberately mid-week and mid-day so boundaries do
					// not line up conveniently with the window edges.
					from := mustTime(t, "2026-02-11T13:47:00Z")
					to := from.AddDate(0, 0, 400)

					intervals := r.Intervals(from, to)
					if len(intervals) == 0 {
						t.Fatal("no coverage at all")
					}

					if !intervals[0].Start.Equal(from) {
						t.Errorf("coverage begins at %s, leaving %s uncovered",
							intervals[0].Start, from)
					}
					if last := intervals[len(intervals)-1]; !last.End.Equal(to) {
						t.Errorf("coverage ends at %s, leaving a gap to %s", last.End, to)
					}
					for i := 1; i < len(intervals); i++ {
						prev, cur := intervals[i-1], intervals[i]
						switch {
						case cur.Start.After(prev.End):
							t.Fatalf("GAP of %v between %s and %s - nobody would be paged",
								cur.Start.Sub(prev.End), prev, cur)
						case cur.Start.Before(prev.End):
							t.Fatalf("OVERLAP of %v between %s and %s - two people both on call",
								prev.End.Sub(cur.Start), prev, cur)
						}
					}

					if gaps := r.Gaps(from, to); len(gaps) != 0 {
						t.Errorf("Gaps reported %d holes in a fully covered window: %v",
							len(gaps), gaps)
					}
				})
			}
		}
	}
}

// Every shift in a rotation with no restrictions should be the same length in
// calendar terms, even where a DST transition makes it 167 or 169 hours.
func TestShiftLengthsAreCalendarWeeksNotFixedDurations(t *testing.T) {
	r, _ := build(t, `
schedules:
  - name: oncall
    team: platform
    timezone: "America/New_York"
    layers:
      - name: base
        type: rotation
        participants: [sarthak, priya]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
`)
	// A window spanning the March 2026 spring-forward.
	from := mustTime(t, "2026-02-16T14:00:00Z")
	to := mustTime(t, "2026-04-20T14:00:00Z")

	intervals := r.Intervals(from, to)
	var sawShortWeek bool
	loc, _ := time.LoadLocation("America/New_York")

	for _, iv := range intervals {
		// Ignore the truncated first and last spans.
		if iv.Start.Equal(from) || iv.End.Equal(to) {
			continue
		}
		switch iv.Duration() {
		case 7 * 24 * time.Hour:
		case 167 * time.Hour:
			sawShortWeek = true
		case 169 * time.Hour:
		default:
			t.Errorf("shift %s has length %v, want 168h, 167h or 169h", iv, iv.Duration())
		}
		// Whatever the length, it must start at 09:00 local.
		if local := iv.Start.In(loc); local.Hour() != 9 || local.Minute() != 0 {
			t.Errorf("shift starts at %s local, want 09:00", local.Format("15:04"))
		}
	}
	if !sawShortWeek {
		t.Error("no 167-hour week observed; the window did not cross the transition")
	}
}

// ─── Overrides ────────────────────────────────────────────────────────────

func override(t *testing.T, user, from, to string, created string) core.Override {
	t.Helper()
	return core.Override{
		ScheduleName: "oncall",
		UserID:       user,
		StartsAt:     mustTime(t, from),
		EndsAt:       mustTime(t, to),
		CreatedAt:    mustTime(t, created),
	}
}

// "Arun covers for Priya this Thursday" is state, not config: forcing a git
// commit for a last-minute swap would be user-hostile.
func TestOverrideBeatsTheRotation(t *testing.T) {
	ov := override(t, "arun",
		"2026-08-20T12:00:00Z", "2026-08-21T03:30:00Z", "2026-08-18T00:00:00Z")
	r, _ := build(t, weeklyTwo, ov)

	inside := mustTime(t, "2026-08-20T18:00:00Z")
	if got, _ := r.At(inside); got != "arun" {
		t.Errorf("during the override At = %q, want arun", got)
	}

	before := mustTime(t, "2026-08-20T11:00:00Z")
	if got, _ := r.At(before); got == "arun" {
		t.Error("the override applied before it started")
	}
	after := mustTime(t, "2026-08-21T04:00:00Z")
	if got, _ := r.At(after); got == "arun" {
		t.Error("the override applied after it ended")
	}
}

func TestOverrideSplitsTheIntervals(t *testing.T) {
	ov := override(t, "arun",
		"2026-08-20T12:00:00Z", "2026-08-21T00:00:00Z", "2026-08-18T00:00:00Z")
	r, _ := build(t, weeklyTwo, ov)

	from := mustTime(t, "2026-08-17T03:30:00Z")
	to := mustTime(t, "2026-08-24T03:30:00Z")
	intervals := r.Intervals(from, to)

	var sawArun bool
	for i, iv := range intervals {
		if iv.UserID == "arun" {
			sawArun = true
			if !iv.Start.Equal(ov.StartsAt) || !iv.End.Equal(ov.EndsAt) {
				t.Errorf("override interval is %s, want exactly the override window", iv)
			}
		}
		if i > 0 && !iv.Start.Equal(intervals[i-1].End) {
			t.Errorf("override introduced a discontinuity between %s and %s", intervals[i-1], iv)
		}
	}
	if !sawArun {
		t.Fatal("the override does not appear in the intervals")
	}
	// The window is still fully covered.
	if gaps := r.Gaps(from, to); len(gaps) != 0 {
		t.Errorf("override left %d gaps: %v", len(gaps), gaps)
	}
}

// Where two overrides collide the later-created one wins, so the outcome does
// not depend on the order rows came back from the database.
func TestLaterCreatedOverrideWins(t *testing.T) {
	early := override(t, "arun",
		"2026-08-20T12:00:00Z", "2026-08-21T00:00:00Z", "2026-08-18T00:00:00Z")
	late := override(t, "priya",
		"2026-08-20T12:00:00Z", "2026-08-21T00:00:00Z", "2026-08-19T00:00:00Z")

	forward, _ := build(t, weeklyTwo, early, late)
	reversed, _ := build(t, weeklyTwo, late, early)

	at := mustTime(t, "2026-08-20T18:00:00Z")
	a, _ := forward.At(at)
	b, _ := reversed.At(at)

	if a != "priya" || b != "priya" {
		t.Errorf("later-created override should win regardless of input order; got %q and %q", a, b)
	}
}

func TestOverrideForAnotherScheduleIsIgnored(t *testing.T) {
	ov := override(t, "arun",
		"2026-08-20T12:00:00Z", "2026-08-21T00:00:00Z", "2026-08-18T00:00:00Z")
	ov.ScheduleName = "some-other-schedule"
	r, _ := build(t, weeklyTwo, ov)

	if got, _ := r.At(mustTime(t, "2026-08-20T18:00:00Z")); got == "arun" {
		t.Error("an override for a different schedule was applied")
	}
}

// ─── Gaps ─────────────────────────────────────────────────────────────────

// An unnoticed gap means an unpaged incident, so Gaps must actually be able to
// find one.
func TestGapsFindsAnUncoveredWindow(t *testing.T) {
	r, s := build(t, weeklyTwo)
	// A schedule with no layers covers nothing.
	s.Layers = nil

	from := mustTime(t, "2026-08-01T00:00:00Z")
	to := mustTime(t, "2026-08-08T00:00:00Z")

	if got := r.Intervals(from, to); len(got) != 0 {
		t.Fatalf("a schedule with no layers produced coverage: %v", got)
	}
	gaps := r.Gaps(from, to)
	if len(gaps) != 1 {
		t.Fatalf("got %d gaps, want 1 covering the whole window", len(gaps))
	}
	if !gaps[0].Start.Equal(from) || !gaps[0].End.Equal(to) {
		t.Errorf("gap = %s, want the whole window", gaps[0])
	}
}

func TestEmptyWindowProducesNothing(t *testing.T) {
	r, _ := build(t, weeklyTwo)
	at := mustTime(t, "2026-08-17T00:00:00Z")
	if got := r.Intervals(at, at); len(got) != 0 {
		t.Errorf("a zero-length window produced %d intervals", len(got))
	}
	if got := r.Intervals(at.Add(time.Hour), at); len(got) != 0 {
		t.Errorf("a reversed window produced %d intervals", len(got))
	}
}
