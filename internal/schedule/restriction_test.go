package schedule_test

import (
	"testing"
	"time"
)

// businessHoursOnly has cover on weekday daytimes and nothing behind it, so it
// is deliberately full of holes.
const businessHoursOnly = `
schedules:
  - name: oncall
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: business-hours
        type: restriction
        participants: [sarthak, priya]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
        restriction:
          days: [monday, tuesday, wednesday, thursday, friday]
          start: "09:00"
          end: "18:00"
`

// A restriction is what makes a coverage gap possible at all: a bare rotation
// always covers 24x7, so without this the gap detector has nothing to find.
func TestRestrictionLeavesNightsAndWeekendsUncovered(t *testing.T) {
	r, _ := build(t, businessHoursOnly)
	loc, _ := time.LoadLocation("Asia/Kolkata")

	covered := []string{
		"2026-08-17T10:00:00+05:30", // Monday morning
		"2026-08-19T17:59:00+05:30", // Wednesday, just inside
		"2026-08-21T09:00:00+05:30", // Friday, exactly at the opening boundary
	}
	uncovered := []string{
		"2026-08-17T08:59:00+05:30", // just before opening
		"2026-08-17T18:00:00+05:30", // exactly at close: the window is half-open
		"2026-08-18T03:00:00+05:30", // the middle of the night
		"2026-08-22T12:00:00+05:30", // Saturday
		"2026-08-23T12:00:00+05:30", // Sunday
	}

	for _, s := range covered {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if _, ok := r.At(at); !ok {
			t.Errorf("%s should be covered but nobody is on call", at.In(loc))
		}
	}
	for _, s := range uncovered {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if user, ok := r.At(at); ok {
			t.Errorf("%s should be uncovered but reports %s", at.In(loc), user)
		}
	}
}

func TestGapsAreReportedForABusinessHoursSchedule(t *testing.T) {
	r, _ := build(t, businessHoursOnly)

	from := mustTime(t, "2026-08-17T03:30:00Z") // Monday 09:00 IST
	to := mustTime(t, "2026-08-24T03:30:00Z")   // the following Monday

	gaps := r.Gaps(from, to)
	if len(gaps) == 0 {
		t.Fatal("a weekday-daytime-only schedule should report gaps")
	}

	// Nights on Monday through Thursday, plus one long stretch from Friday
	// evening to Monday morning: five holes in a week.
	if len(gaps) != 5 {
		t.Errorf("got %d gaps, want 5 (four nights and the weekend): %v", len(gaps), gaps)
	}

	var longest time.Duration
	for _, g := range gaps {
		if g.Duration() > longest {
			longest = g.Duration()
		}
	}
	// Friday 18:00 to Monday 09:00 is 63 hours.
	if longest != 63*time.Hour {
		t.Errorf("longest gap = %v, want 63h across the weekend", longest)
	}
}

// Coverage and gaps must partition the window exactly. Anything else means the
// gap detector and the calendar disagree about the same schedule, and one of
// them is lying to the operator.
func TestIntervalsAndGapsPartitionTheWindow(t *testing.T) {
	r, _ := build(t, businessHoursOnly)
	from := mustTime(t, "2026-08-17T00:00:00Z")
	to := from.AddDate(0, 0, 30)

	spans := append(r.Intervals(from, to), r.Gaps(from, to)...)
	if len(spans) == 0 {
		t.Fatal("no spans at all")
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[j].Start.Before(spans[i].Start) {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
	}
	if !spans[0].Start.Equal(from) {
		t.Errorf("coverage starts at %s, want %s", spans[0].Start, from)
	}
	for i := 1; i < len(spans); i++ {
		if !spans[i].Start.Equal(spans[i-1].End) {
			t.Fatalf("window is not tiled: %s then %s", spans[i-1], spans[i])
		}
	}
	if last := spans[len(spans)-1]; !last.End.Equal(to) {
		t.Errorf("coverage ends at %s, want %s", last.End, to)
	}
}

// An after-hours window runs past midnight, so it must still be open at three
// in the morning the following day.
func TestWindowCrossingMidnightStaysOpen(t *testing.T) {
	r, _ := build(t, `
schedules:
  - name: oncall
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: after-hours
        type: restriction
        participants: [sarthak]
        restriction:
          start: "18:00"
          end: "09:00"
`)
	cases := []struct {
		at   string
		want bool
	}{
		{"2026-08-17T17:59:00+05:30", false}, // before it opens
		{"2026-08-17T18:00:00+05:30", true},  // opens
		{"2026-08-17T23:59:00+05:30", true},  // before midnight
		{"2026-08-18T03:00:00+05:30", true},  // after midnight, still open
		{"2026-08-18T08:59:00+05:30", true},  // just before it closes
		{"2026-08-18T09:00:00+05:30", false}, // closes; half-open
		{"2026-08-18T12:00:00+05:30", false}, // daytime
	}
	for _, c := range cases {
		at, err := time.Parse(time.RFC3339, c.at)
		if err != nil {
			t.Fatalf("parse %q: %v", c.at, err)
		}
		if _, ok := r.At(at); ok != c.want {
			t.Errorf("%s covered = %v, want %v", c.at, ok, c.want)
		}
	}
}

// Business hours over a 24x7 rotation is the arrangement the spec describes,
// and together they must leave nothing uncovered.
func TestLayeredRestrictionsCoverTheWholeWeek(t *testing.T) {
	r, _ := build(t, `
schedules:
  - name: oncall
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: business-hours
        type: restriction
        participants: [sarthak, priya]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
        restriction:
          days: [monday, tuesday, wednesday, thursday, friday]
          start: "09:00"
          end: "18:00"
      - name: everything-else
        type: rotation
        participants: [arun]
        rotation: weekly
        handoff:
          day: monday
          time: "09:00"
`)
	from := mustTime(t, "2026-08-17T00:00:00Z")
	to := from.AddDate(0, 0, 60)

	if gaps := r.Gaps(from, to); len(gaps) != 0 {
		t.Errorf("a business-hours layer backed by a 24x7 rotation left %d gaps: %v",
			len(gaps), gaps)
	}

	// The restricted layer is listed first, so it wins during business hours
	// and the fallback covers everything else.
	weekdayNoon := mustTime(t, "2026-08-19T06:30:00Z") // Wednesday 12:00 IST
	night := mustTime(t, "2026-08-19T21:00:00Z")       // Thursday 02:30 IST

	day, _ := r.At(weekdayNoon)
	off, _ := r.At(night)

	if day == "arun" {
		t.Error("the fallback layer answered during business hours; layer order is not respected")
	}
	if off != "arun" {
		t.Errorf("night resolved to %q, want the fallback participant arun", off)
	}
}

// The restriction window is wall clock, so it must not drift across DST.
func TestRestrictionWindowHoldsItsWallClockAcrossDST(t *testing.T) {
	r, _ := build(t, `
schedules:
  - name: oncall
    team: platform
    timezone: "America/New_York"
    layers:
      - name: business-hours
        type: restriction
        participants: [sarthak]
        restriction:
          days: [monday, tuesday, wednesday, thursday, friday]
          start: "09:00"
          end: "17:00"
`)
	loc, _ := time.LoadLocation("America/New_York")

	// Span the March 2026 spring-forward.
	from := mustTime(t, "2026-03-02T00:00:00Z")
	to := mustTime(t, "2026-03-20T00:00:00Z")

	var sawEDT, sawEST bool
	for _, iv := range r.Intervals(from, to) {
		start := iv.Start.In(loc)
		end := iv.End.In(loc)
		if start.Hour() != 9 || start.Minute() != 0 {
			t.Errorf("window opens at %s local, want 09:00", start.Format("15:04"))
		}
		if end.Hour() != 17 || end.Minute() != 0 {
			t.Errorf("window closes at %s local, want 17:00", end.Format("15:04"))
		}
		switch zone, _ := start.Zone(); zone {
		case "EDT":
			sawEDT = true
		case "EST":
			sawEST = true
		}
	}
	if !sawEST || !sawEDT {
		t.Error("the window did not span the transition; the test proves nothing")
	}
}

// Days restricts which days the window opens on at all.
func TestRestrictionDaysAreHonoured(t *testing.T) {
	r, _ := build(t, `
schedules:
  - name: oncall
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: weekends-only
        type: restriction
        participants: [sarthak]
        restriction:
          days: [saturday, sunday]
          start: "00:00"
          end: "23:59"
`)
	saturday := mustTime(t, "2026-08-22T06:00:00Z") // Sat 11:30 IST
	wednesday := mustTime(t, "2026-08-19T06:00:00Z")

	if _, ok := r.At(saturday); !ok {
		t.Error("Saturday should be covered by a weekends-only layer")
	}
	if _, ok := r.At(wednesday); ok {
		t.Error("Wednesday should not be covered by a weekends-only layer")
	}
}
