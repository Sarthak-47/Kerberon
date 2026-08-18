package schedule

import (
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/config"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// Restriction confines a layer to a recurring window of local time.
//
// A layer with a restriction puts nobody on call outside it, which is how a
// schedule ends up with a coverage gap. That is the point: business-hours cover
// with no after-hours layer behind it is a real hole, and kerberon validate
// exists to find it before an incident does.
type Restriction struct {
	// Days the window applies on. Empty means every day.
	Days map[time.Weekday]bool
	// StartHour/StartMinute and EndHour/EndMinute bound the window in local
	// wall-clock terms.
	StartHour, StartMinute int
	EndHour, EndMinute     int
	// CrossesMidnight is true when End is at or before Start, e.g. an
	// after-hours window running 18:00 to 09:00.
	CrossesMidnight bool
}

// NewRestriction converts the configured form.
func NewRestriction(c *config.Restriction) (*Restriction, error) {
	if c == nil {
		return nil, nil
	}

	sh, sm, err := parseHHMM(c.Start)
	if err != nil {
		return nil, fmt.Errorf("restriction start: %w", err)
	}
	eh, em, err := parseHHMM(c.End)
	if err != nil {
		return nil, fmt.Errorf("restriction end: %w", err)
	}

	r := &Restriction{
		StartHour: sh, StartMinute: sm,
		EndHour: eh, EndMinute: em,
		CrossesMidnight: eh*60+em <= sh*60+sm,
	}
	if len(c.Days) > 0 {
		r.Days = make(map[time.Weekday]bool, len(c.Days))
		for _, d := range c.Days {
			wd, err := parseWeekday(d)
			if err != nil {
				return nil, fmt.Errorf("restriction days: %w", err)
			}
			r.Days[wd] = true
		}
	}
	return r, nil
}

// windowStartingOn builds the window whose start falls on the given local date.
//
// The window is anchored by its start day: a Friday 18:00 to 09:00 window runs
// into Saturday morning and still counts as Friday's, which is what an operator
// means by "Friday night".
func (r *Restriction) windowStartingOn(day time.Time, loc *time.Location) (core.Interval, bool) {
	if r.Days != nil && !r.Days[day.In(loc).Weekday()] {
		return core.Interval{}, false
	}

	d := day.In(loc)
	start := ResolveInstant(WallClock{
		Year: d.Year(), Month: d.Month(), Day: d.Day(),
		Hour: r.StartHour, Minute: r.StartMinute,
	}, loc)

	endDay := d
	if r.CrossesMidnight {
		endDay = d.AddDate(0, 0, 1)
	}
	end := ResolveInstant(WallClock{
		Year: endDay.Year(), Month: endDay.Month(), Day: endDay.Day(),
		Hour: r.EndHour, Minute: r.EndMinute,
	}, loc)

	if !end.After(start) {
		return core.Interval{}, false
	}
	return core.Interval{Start: start, End: end}, true
}

// covers reports whether t falls inside a window.
//
// Yesterday is checked too, because a window crossing midnight is still open in
// the small hours of the following day.
func (r *Restriction) covers(t time.Time, loc *time.Location) bool {
	local := t.In(loc)
	for _, offset := range []int{-1, 0} {
		day := local.AddDate(0, 0, offset)
		if w, ok := r.windowStartingOn(day, loc); ok && w.Contains(t) {
			return true
		}
	}
	return false
}

// boundaries lists the window edges falling in (from, to].
func (r *Restriction) boundaries(from, to time.Time, loc *time.Location) []time.Time {
	var out []time.Time

	// Start a day early so a window opened yesterday contributes its closing
	// edge, and end a day late so one opening on the final day contributes its
	// opening edge.
	day := from.In(loc).AddDate(0, 0, -1)
	limit := to.In(loc).AddDate(0, 0, 1)

	for ; day.Before(limit); day = day.AddDate(0, 0, 1) {
		w, ok := r.windowStartingOn(day, loc)
		if !ok {
			continue
		}
		for _, edge := range []time.Time{w.Start, w.End} {
			if edge.After(from) && !edge.After(to) {
				out = append(out, edge)
			}
		}
	}
	return out
}
