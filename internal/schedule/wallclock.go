// Package schedule resolves who is on call at a given instant.
//
// Rotations are defined as wall-clock time in a named IANA timezone, never as a
// fixed UTC offset. "+05:30" is not a timezone; Asia/Kolkata is. An offset
// cannot express daylight saving, so a rotation built on one silently drifts
// twice a year.
//
// Every occurrence is computed with time.Date in the target location and then
// converted to UTC. Adding 7*24h to get the next weekly handoff is wrong:
// across a DST transition a week is 167 or 169 hours, and the naive version
// produces a coverage gap or an overlap exactly twice a year — the two moments
// nobody is watching for it.
package schedule

import (
	"fmt"
	"time"
)

// Two local times a year are pathological, and both policies below are
// deliberate, documented, and directly tested against real historical
// transitions.

// AmbiguousPolicy and NonexistentPolicy describe what happens to a handoff that
// falls in a DST discontinuity. They exist as named concepts rather than
// buried behaviour because an operator debugging a missed handoff needs to be
// able to look the rule up.
const (
	// PolicyAmbiguous: when the clock goes back, a wall-clock time occurs
	// twice. Kerberon takes the FIRST occurrence, so the handoff happens as
	// early as it legitimately can and nobody is left uncovered in between.
	PolicyAmbiguous = "first occurrence"

	// PolicyNonexistent: when the clock springs forward, a wall-clock time may
	// not occur at all. Kerberon shifts to the FIRST VALID INSTANT after the
	// gap — the moment the offset changes, not the requested time plus the
	// gap. A 02:30 handoff on a night where 02:00–03:00 vanishes happens at
	// 03:00, not 03:30.
	PolicyNonexistent = "first valid instant after the gap"
)

// WallClock is a local date and time-of-day, independent of any offset.
type WallClock struct {
	Year   int
	Month  time.Month
	Day    int
	Hour   int
	Minute int
}

func (w WallClock) String() string {
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d", w.Year, w.Month, w.Day, w.Hour, w.Minute)
}

// Resolution records how a wall clock mapped onto the timeline.
type Resolution struct {
	// Instant is the UTC instant the wall clock resolved to.
	Instant time.Time
	// Ambiguous is true when the wall clock occurred twice and the earlier was
	// chosen.
	Ambiguous bool
	// Nonexistent is true when the wall clock was skipped and the instant was
	// moved to the end of the gap.
	Nonexistent bool
}

// Resolve maps a wall clock in loc onto a single instant, applying the DST
// policies above.
func Resolve(w WallClock, loc *time.Location) Resolution {
	t := time.Date(w.Year, w.Month, w.Day, w.Hour, w.Minute, 0, 0, loc)

	if !matches(t, w) {
		// The requested wall clock does not exist. time.Date normalizes it,
		// but the direction it lands in is not something to rely on — for a
		// spring-forward it can come back *before* the gap. Find the
		// transition on that local date directly.
		if end, ok := transitionOnDate(w, loc); ok {
			return Resolution{Instant: end, Nonexistent: true}
		}
		return Resolution{Instant: t, Nonexistent: true}
	}

	// The wall clock exists. It may still occur twice. Check both directions,
	// because which of the two occurrences time.Date returns is not specified.
	for _, shift := range []time.Duration{time.Hour, 30 * time.Minute} {
		if matches(t.Add(-shift), w) {
			// t is the second occurrence; the earlier one is the first.
			return Resolution{Instant: t.Add(-shift), Ambiguous: true}
		}
		if matches(t.Add(shift), w) {
			// t is already the first occurrence.
			return Resolution{Instant: t, Ambiguous: true}
		}
	}

	return Resolution{Instant: t}
}

// transitionOnDate finds the instant the UTC offset changes on the local date
// described by w, which for a spring-forward is exactly the instant the gap
// closes and therefore the first valid time after it.
//
// The offset is constant either side of a single transition, so bisection
// finds the boundary.
func transitionOnDate(w WallClock, loc *time.Location) (time.Time, bool) {
	// Anchor at local noon and step back twelve hours, so the start of the
	// window is well-defined even in a zone where midnight itself is skipped.
	noon := time.Date(w.Year, w.Month, w.Day, 12, 0, 0, 0, loc)
	lo := noon.Add(-12 * time.Hour)
	hi := noon.Add(14 * time.Hour)

	_, loOff := lo.In(loc).Zone()
	_, hiOff := hi.In(loc).Zone()
	if loOff == hiOff {
		return time.Time{}, false
	}

	// Invariant: lo carries the old offset, hi the new one.
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.In(loc).Zone(); off == hiOff {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi.Truncate(time.Second), true
}

// ResolveInstant is Resolve when only the instant is wanted.
func ResolveInstant(w WallClock, loc *time.Location) time.Time {
	return Resolve(w, loc).Instant
}

// matches reports whether t's local representation equals the requested wall
// clock.
func matches(t time.Time, w WallClock) bool {
	return t.Year() == w.Year &&
		t.Month() == w.Month &&
		t.Day() == w.Day &&
		t.Hour() == w.Hour &&
		t.Minute() == w.Minute
}

// NextWeekly returns the first occurrence of weekday at hh:mm in loc strictly
// after `after`.
//
// It walks calendar days and constructs each candidate with time.Date, so a
// week that is 167 or 169 hours long is handled by the calendar rather than by
// arithmetic on durations.
func NextWeekly(after time.Time, weekday time.Weekday, hour, minute int, loc *time.Location) time.Time {
	local := after.In(loc)

	// Start from today and walk forward at most eight days, which is enough to
	// find the next occurrence of any weekday even if today's has passed.
	for i := 0; i <= 8; i++ {
		d := local.AddDate(0, 0, i)
		if d.Weekday() != weekday {
			continue
		}
		candidate := ResolveInstant(WallClock{
			Year: d.Year(), Month: d.Month(), Day: d.Day(),
			Hour: hour, Minute: minute,
		}, loc)
		if candidate.After(after) {
			return candidate
		}
	}
	// Unreachable for a valid weekday, but returning a zero time would be a
	// silent infinite loop upstream.
	panic(fmt.Sprintf("schedule: no %s at %02d:%02d found after %s in %s",
		weekday, hour, minute, after, loc))
}

// NextDaily returns the first occurrence of hh:mm in loc strictly after
// `after`.
func NextDaily(after time.Time, hour, minute int, loc *time.Location) time.Time {
	local := after.In(loc)
	for i := 0; i <= 2; i++ {
		d := local.AddDate(0, 0, i)
		candidate := ResolveInstant(WallClock{
			Year: d.Year(), Month: d.Month(), Day: d.Day(),
			Hour: hour, Minute: minute,
		}, loc)
		if candidate.After(after) {
			return candidate
		}
	}
	panic(fmt.Sprintf("schedule: no %02d:%02d found after %s in %s", hour, minute, after, loc))
}
