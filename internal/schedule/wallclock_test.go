package schedule_test

import (
	"testing"
	"time"

	_ "time/tzdata"

	"github.com/Sarthak-47/kerberon/internal/schedule"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// The tzdata import above must actually be doing something, or these tests
// pass on a developer machine and the binary breaks on a minimal container
// image that ships no zoneinfo.
func TestTimezoneDatabaseIsEmbedded(t *testing.T) {
	for _, name := range []string{
		"America/New_York", "Europe/London", "Australia/Sydney", "Asia/Kolkata",
	} {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("zone %s unavailable: %v", name, err)
		}
	}
}

// ─── Ordinary days ────────────────────────────────────────────────────────

func TestResolveOnAnOrdinaryDay(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	got := schedule.Resolve(schedule.WallClock{
		Year: 2026, Month: time.June, Day: 15, Hour: 9, Minute: 0,
	}, loc)

	if got.Ambiguous || got.Nonexistent {
		t.Errorf("an ordinary day should be neither ambiguous nor nonexistent: %+v", got)
	}
	want := time.Date(2026, time.June, 15, 13, 0, 0, 0, time.UTC) // EDT = UTC-4
	if !got.Instant.Equal(want) {
		t.Errorf("instant = %s, want %s", got.Instant.UTC(), want)
	}
}

// ─── Spring forward: nonexistent local times ──────────────────────────────

// On 8 March 2026 New York skips 02:00–03:00. A 02:30 handoff does not exist,
// and the policy is the first valid instant after the gap: 03:00, not 03:30.
func TestNonexistentLocalTimeShiftsToTheEndOfTheGap(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	got := schedule.Resolve(schedule.WallClock{
		Year: 2026, Month: time.March, Day: 8, Hour: 2, Minute: 30,
	}, loc)

	if !got.Nonexistent {
		t.Fatalf("02:30 on a spring-forward date should be reported nonexistent: %+v", got)
	}
	local := got.Instant.In(loc)
	if local.Hour() != 3 || local.Minute() != 0 {
		t.Errorf("resolved to %s, want 03:00 local (the first valid instant)", local.Format("15:04"))
	}
	// 07:00 UTC is when the offset changes from EST to EDT.
	want := time.Date(2026, time.March, 8, 7, 0, 0, 0, time.UTC)
	if !got.Instant.Equal(want) {
		t.Errorf("instant = %s, want %s", got.Instant.UTC(), want)
	}
}

func TestNonexistentLocalTimeAcrossZones(t *testing.T) {
	cases := []struct {
		zone       string
		w          schedule.WallClock
		wantLocalH int
		wantLocalM int
	}{
		// Europe/London springs forward at 01:00 GMT on 29 March 2026.
		{"Europe/London", schedule.WallClock{Year: 2026, Month: time.March, Day: 29, Hour: 1, Minute: 30}, 2, 0},
		// Australia/Sydney springs forward at 02:00 on 4 October 2026
		// (southern hemisphere, opposite direction from the others).
		{"Australia/Sydney", schedule.WallClock{Year: 2026, Month: time.October, Day: 4, Hour: 2, Minute: 30}, 3, 0},
	}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			loc := mustLoad(t, c.zone)
			got := schedule.Resolve(c.w, loc)
			if !got.Nonexistent {
				t.Fatalf("%s should be nonexistent in %s: %+v", c.w, c.zone, got)
			}
			local := got.Instant.In(loc)
			if local.Hour() != c.wantLocalH || local.Minute() != c.wantLocalM {
				t.Errorf("resolved to %s, want %02d:%02d",
					local.Format("15:04"), c.wantLocalH, c.wantLocalM)
			}
		})
	}
}

// ─── Fall back: ambiguous local times ─────────────────────────────────────

// On 1 November 2026 New York repeats 01:00–02:00. A 01:30 handoff occurs
// twice, and the policy is the first occurrence — while still on EDT.
func TestAmbiguousLocalTimeTakesTheFirstOccurrence(t *testing.T) {
	loc := mustLoad(t, "America/New_York")
	got := schedule.Resolve(schedule.WallClock{
		Year: 2026, Month: time.November, Day: 1, Hour: 1, Minute: 30,
	}, loc)

	if !got.Ambiguous {
		t.Fatalf("01:30 on a fall-back date should be reported ambiguous: %+v", got)
	}
	// The first occurrence is EDT (UTC-4) = 05:30 UTC. The second would be
	// EST (UTC-5) = 06:30 UTC.
	want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC)
	if !got.Instant.Equal(want) {
		t.Errorf("instant = %s, want the earlier occurrence %s", got.Instant.UTC(), want)
	}
	if name, _ := got.Instant.In(loc).Zone(); name != "EDT" {
		t.Errorf("zone = %s, want EDT (the first occurrence)", name)
	}
}

func TestAmbiguousLocalTimeAcrossZones(t *testing.T) {
	cases := []struct {
		zone string
		w    schedule.WallClock
	}{
		// Europe/London falls back at 02:00 BST on 25 October 2026.
		{"Europe/London", schedule.WallClock{Year: 2026, Month: time.October, Day: 25, Hour: 1, Minute: 30}},
		// Australia/Sydney falls back at 03:00 on 5 April 2026.
		{"Australia/Sydney", schedule.WallClock{Year: 2026, Month: time.April, Day: 5, Hour: 2, Minute: 30}},
	}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			loc := mustLoad(t, c.zone)
			got := schedule.Resolve(c.w, loc)
			if !got.Ambiguous {
				t.Fatalf("%s should be ambiguous in %s: %+v", c.w, c.zone, got)
			}
			// The chosen instant must be the earlier of the two candidates.
			later := got.Instant.Add(time.Hour)
			if later.In(loc).Hour() != c.w.Hour || later.In(loc).Minute() != c.w.Minute {
				t.Errorf("the hour does not appear to repeat; chosen %s", got.Instant.In(loc))
			}
			if !got.Instant.Before(later) {
				t.Error("did not choose the first occurrence")
			}
		})
	}
}

// Asia/Kolkata is the control: a half-hour offset and no DST at all. If a bug
// makes DST handling fire here, this catches it.
func TestKolkataHasNoDiscontinuities(t *testing.T) {
	loc := mustLoad(t, "Asia/Kolkata")

	// Walk a whole year of 02:30 handoffs, the time that breaks elsewhere.
	for d := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC); d.Year() == 2026; d = d.AddDate(0, 0, 1) {
		got := schedule.Resolve(schedule.WallClock{
			Year: d.Year(), Month: d.Month(), Day: d.Day(), Hour: 2, Minute: 30,
		}, loc)
		if got.Ambiguous || got.Nonexistent {
			t.Fatalf("%s reported a discontinuity in a zone that has none: %+v",
				d.Format("2006-01-02"), got)
		}
		if local := got.Instant.In(loc); local.Hour() != 2 || local.Minute() != 30 {
			t.Fatalf("%s resolved to %s, want 02:30", d.Format("2006-01-02"), local.Format("15:04"))
		}
	}
}

// ─── Weekly handoffs ──────────────────────────────────────────────────────

// The bug this guards against: computing the next handoff by adding
// 7*24h. Across a DST transition a week is 167 or 169 hours, so the naive
// version drifts by an hour and the handoff stops happening at 09:00 local.
func TestWeeklyHandoffKeepsItsWallClockAcrossDST(t *testing.T) {
	loc := mustLoad(t, "America/New_York")

	// Start before the March 2026 spring-forward and walk a few weeks past it.
	cursor := time.Date(2026, time.February, 20, 0, 0, 0, 0, time.UTC)
	crossed := false

	for i := 0; i < 6; i++ {
		next := schedule.NextWeekly(cursor, time.Monday, 9, 0, loc)
		local := next.In(loc)

		if local.Weekday() != time.Monday {
			t.Fatalf("handoff %d landed on %s, want Monday", i, local.Weekday())
		}
		if local.Hour() != 9 || local.Minute() != 0 {
			t.Fatalf("handoff %d is at %s local, want 09:00 - the wall clock drifted",
				i, local.Format("15:04"))
		}
		if zone, _ := local.Zone(); zone == "EDT" {
			crossed = true
		}
		cursor = next
	}
	if !crossed {
		t.Error("the window did not actually cross the DST boundary; the test proves nothing")
	}
}

// A week containing a transition really is 167 or 169 hours. Asserting this
// documents why duration arithmetic cannot be used.
func TestWeekLengthVariesAcrossDST(t *testing.T) {
	loc := mustLoad(t, "America/New_York")

	before := schedule.NextWeekly(
		time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), time.Monday, 9, 0, loc)
	after := schedule.NextWeekly(before, time.Monday, 9, 0, loc)

	got := after.Sub(before)
	if got == 7*24*time.Hour {
		t.Errorf("the week spanning the transition was exactly 168h; the transition was not crossed")
	}
	if got != 167*time.Hour {
		t.Errorf("week length = %v, want 167h across a spring-forward", got)
	}
}

func TestWeeklyHandoffIsStrictlyAfter(t *testing.T) {
	loc := mustLoad(t, "Asia/Kolkata")
	first := schedule.NextWeekly(
		time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC), time.Monday, 9, 0, loc)

	// Asking again from exactly the handoff instant must advance a week, not
	// return the same instant, or a rotation would never progress.
	second := schedule.NextWeekly(first, time.Monday, 9, 0, loc)
	if !second.After(first) {
		t.Fatalf("second handoff %s is not after the first %s", second, first)
	}
	if got := second.Sub(first); got != 7*24*time.Hour {
		t.Errorf("gap = %v, want 168h in a zone without DST", got)
	}
}

func TestDailyHandoffKeepsItsWallClockAcrossDST(t *testing.T) {
	loc := mustLoad(t, "Europe/London")
	cursor := time.Date(2026, time.March, 27, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		next := schedule.NextDaily(cursor, 9, 0, loc)
		local := next.In(loc)
		if local.Hour() != 9 || local.Minute() != 0 {
			t.Fatalf("daily handoff %d is at %s local, want 09:00", i, local.Format("15:04"))
		}
		cursor = next
	}
}

// Handoffs must never repeat or reverse, which would double-book or skip a
// rotation slot.
func TestHandoffsAreStrictlyIncreasingAcrossAYear(t *testing.T) {
	for _, zone := range []string{
		"America/New_York", "Europe/London", "Australia/Sydney", "Asia/Kolkata",
	} {
		t.Run(zone, func(t *testing.T) {
			loc := mustLoad(t, zone)
			cursor := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			end := cursor.AddDate(1, 0, 0)

			prev := cursor
			for cursor.Before(end) {
				next := schedule.NextWeekly(cursor, time.Monday, 9, 0, loc)
				if !next.After(prev) {
					t.Fatalf("handoff %s did not advance past %s", next, prev)
				}
				local := next.In(loc)
				if local.Weekday() != time.Monday || local.Hour() != 9 {
					t.Fatalf("handoff %s is not Monday 09:00 local", local)
				}
				prev, cursor = next, next
			}
		})
	}
}
